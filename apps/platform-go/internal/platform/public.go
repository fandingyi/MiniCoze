package platform

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type publicKey struct{}
type PublicAccess struct {
	VisitorID string `json:"-"`
	Slug      string `json:"-"`
	ChannelID string `json:"channel_id"`
	AgentID   string `json:"agent_id"`
	OwnerID   string `json:"owner_id"`
	Scope     string `json:"scope"`
	APIHash   string `json:"api_hash,omitempty"`
	// Limits are derived from the server-owned publish-channel config. They are
	// intentionally not serialized into the immutable run snapshot.
	RateLimitPerMinute int `json:"-"`
	RateLimitPerDay    int `json:"-"`
}

func publicAccess(ctx context.Context) *PublicAccess {
	v, _ := ctx.Value(publicKey{}).(*PublicAccess)
	return v
}
func (a PublicAccess) prefix() string { return "public:v2:" + a.Scope + ":" }
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func checkPublicAccess(ctx context.Context, q rowQuerier, a *PublicAccess) error {
	var config []byte
	var kind string
	err := q.QueryRowContext(ctx, `SELECT c."config",c."channel" FROM "PublishChannel" c JOIN "Agent" a ON a."id"=c."targetId" AND a."workspaceId"=c."workspaceId" JOIN "User" u ON u."id"=a."creatorId" WHERE c."id"=$1 AND c."targetType"='AGENT' AND c."enabled"=true AND a."id"=$2 AND a."creatorId"=$3 AND a."status"='ACTIVE' AND a."currentVersionId" IS NOT NULL AND u."status"='active'`, a.ChannelID, a.AgentID, a.OwnerID).Scan(&config, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if a.APIHash == "" {
		if kind != "WEB" {
			return ErrForbidden
		}
		return nil
	}
	if kind != "API" {
		return ErrForbidden
	}
	var settings struct {
		Hash    string  `json:"apiKeyHash"`
		Expires *string `json:"expiresAt"`
	}
	if json.Unmarshal(config, &settings) != nil || !hmac.Equal([]byte(settings.Hash), []byte(a.APIHash)) {
		return ErrForbidden
	}
	if settings.Expires != nil && *settings.Expires != "" {
		expiry, err := time.Parse(time.RFC3339Nano, *settings.Expires)
		if err != nil || !time.Now().Before(expiry) {
			return ErrForbidden
		}
	}
	policy, err := publicPolicy(kind, config)
	if err != nil {
		return err
	}
	a.RateLimitPerMinute = policy.RateLimitPerMinute
	a.RateLimitPerDay = policy.RateLimitPerDay
	return nil
}

type PublicTarget struct {
	Access PublicAccess
	Info   json.RawMessage
	Policy PublicPolicy
}

// Publish channel settings are authorization constraints, not UI-only metadata.
// Origin constrains browser callers; IP constraints use the immediate peer only
// because this server does not trust forwarded client-address headers.
type PublicPolicy struct {
	AllowAnonymous     bool
	AllowedOrigins     []string
	AllowedIPs         []string
	RateLimitPerMinute int
	RateLimitPerDay    int
}

func publicPolicy(kind string, raw []byte) (PublicPolicy, error) {
	policy := PublicPolicy{AllowAnonymous: kind == "WEB"}
	if kind == "API" {
		// Keep the Go gateway aligned with the channel-management defaults.
		policy.RateLimitPerMinute = 60
		policy.RateLimitPerDay = 1000
	}
	var config struct {
		AllowAnonymous     *bool    `json:"allowAnonymous"`
		AllowedOrigins     []string `json:"allowedOrigins"`
		AllowedIPs         []string `json:"allowedIps"`
		RateLimitPerMinute *int     `json:"rateLimitPerMinute"`
		RateLimitPerDay    *int     `json:"rateLimitPerDay"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return policy, ErrCapabilities
	}
	if config.AllowAnonymous != nil {
		policy.AllowAnonymous = *config.AllowAnonymous
	}
	if config.RateLimitPerMinute != nil {
		if *config.RateLimitPerMinute < 1 || *config.RateLimitPerMinute > 10000 {
			return policy, ErrCapabilities
		}
		policy.RateLimitPerMinute = *config.RateLimitPerMinute
	}
	if config.RateLimitPerDay != nil {
		if *config.RateLimitPerDay < 1 || *config.RateLimitPerDay > 1000000 {
			return policy, ErrCapabilities
		}
		policy.RateLimitPerDay = *config.RateLimitPerDay
	}
	for _, origin := range config.AllowedOrigins {
		if origin == "" || (origin != "*" && !strings.HasPrefix(origin, "https://") && !strings.HasPrefix(origin, "http://")) {
			return policy, ErrCapabilities
		}
		policy.AllowedOrigins = append(policy.AllowedOrigins, origin)
	}
	for _, cidr := range config.AllowedIPs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			if _, err := netip.ParseAddr(cidr); err != nil {
				return policy, ErrCapabilities
			}
		}
		policy.AllowedIPs = append(policy.AllowedIPs, cidr)
	}
	return policy, nil
}

func (p PublicPolicy) allowsRequest(kind string, request *http.Request) bool {
	if kind == "WEB" && !p.AllowAnonymous {
		return false
	}
	if origin := request.Header.Get("Origin"); origin != "" && len(p.AllowedOrigins) > 0 {
		allowed := false
		for _, value := range p.AllowedOrigins {
			if value == "*" || value == origin {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	if kind != "API" || len(p.AllowedIPs) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return false
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, value := range p.AllowedIPs {
		if prefix, err := netip.ParsePrefix(value); err == nil && prefix.Contains(address) {
			return true
		}
		if exact, err := netip.ParseAddr(value); err == nil && exact == address {
			return true
		}
	}
	return false
}

func (p Postgres) resolvePublic(ctx context.Context, kind, lookup string) (PublicTarget, error) {
	target := PublicTarget{}
	var snapshot []byte
	var config []byte
	rows, err := p.DB.QueryContext(ctx, `SELECT c."id",a."id",a."creatorId",c."config",v."snapshot" FROM "PublishChannel" c JOIN "Agent" a ON a."id"=c."targetId" AND a."workspaceId"=c."workspaceId" JOIN "AgentVersion" v ON v."id"=a."currentVersionId" AND v."agentId"=a."id" WHERE c."targetType"='AGENT' AND c."channel"::text=$1 AND c."enabled"=true AND a."status"='ACTIVE' AND c."config"->>$2=$3 LIMIT 2`, kind, map[string]string{"WEB": "slug", "API": "apiKeyHash"}[kind], lookup)
	if err != nil {
		return target, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if err = rows.Scan(&target.Access.ChannelID, &target.Access.AgentID, &target.Access.OwnerID, &config, &snapshot); err != nil {
			return target, err
		}
	}
	if err = rows.Err(); err != nil {
		return target, err
	}
	if count != 1 {
		return target, ErrNotFound
	}
	if kind == "API" {
		target.Access.APIHash = lookup
	}
	if target.Policy, err = publicPolicy(kind, config); err != nil {
		return target, err
	}
	if err = checkPublicAccess(ctx, p.DB, &target.Access); err != nil {
		return target, err
	}
	var published struct {
		Agent struct {
			Name           string  `json:"name"`
			Description    *string `json:"description"`
			AvatarURL      *string `json:"avatarUrl"`
			OpeningMessage *string `json:"openingMessage"`
			PersistedRuns  bool    `json:"persistedRuns"`
		} `json:"agent"`
	}
	if json.Unmarshal(snapshot, &published) != nil {
		return target, ErrCapabilities
	}
	published.Agent.PersistedRuns = true
	target.Info, _ = json.Marshal(published.Agent)
	return target, nil
}

// Only this handler can construct a public principal; client visitorId is never
// accepted as proof of ownership. API credentials define their own namespace.
func RegisterPublicRoutes(mux *http.ServeMux, store Postgres, sessionSecret string, secure bool) error {
	if len(sessionSecret) < 32 {
		return errors.New("PUBLIC_SESSION_SECRET must be at least 32 characters")
	}
	sign := func(channel, nonce string) string {
		mac := hmac.New(sha256.New, []byte(sessionSecret))
		mac.Write([]byte(channel + ":" + nonce))
		return hex.EncodeToString(mac.Sum(nil))
	}
	handler := func(kind, action string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			lookup := r.PathValue("slug")
			if kind == "API" {
				fields := strings.Fields(r.Header.Get("Authorization"))
				if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || len(fields[1]) < 16 || len(fields[1]) > 512 || len(r.Header.Values("Authorization")) != 1 {
					cancel()
					respond(w, 401, 40100, "API key required", nil)
					return
				}
				lookup = digest(fields[1])
			}
			target, err := store.resolvePublic(ctx, kind, lookup)
			cancel()
			if err != nil {
				if kind == "API" && (errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden)) {
					respond(w, 401, 40100, "Invalid or expired API key", nil)
				} else {
					runHTTPError(w, err)
				}
				return
			}
			if !target.Policy.allowsRequest(kind, r) {
				respond(w, 403, 40300, "Publish channel policy denied this request", nil)
				return
			}
			access := target.Access
			if kind == "WEB" {
				cookieName := "mc_visitor_" + digest(access.ChannelID)[:12]
				nonce := ""
				if cookie, err := r.Cookie(cookieName); err == nil {
					parts := strings.Split(cookie.Value, ".")
					if len(parts) == 3 && len(parts[0]) == 32 {
						expires, parseErr := strconv.ParseInt(parts[1], 10, 64)
						if parseErr == nil && time.Now().Unix() < expires && hmac.Equal([]byte(sign(access.ChannelID, parts[0]+"."+parts[1])), []byte(parts[2])) {
							nonce = parts[0]
						}
					}
				}
				if nonce == "" {
					if action != "info" {
						respond(w, 401, 40100, "Visitor session expired; reopen the application", nil)
						return
					}
					nonce, err = randomID()
					if err != nil {
						respond(w, 503, 50300, "Session unavailable", nil)
						return
					}
					expiry := strconv.FormatInt(time.Now().Add(30*24*time.Hour).Unix(), 10)
					http.SetCookie(w, &http.Cookie{Name: cookieName, Value: nonce + "." + expiry + "." + sign(access.ChannelID, nonce+"."+expiry), Path: "/api/public/agents/" + url.PathEscape(lookup), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: 86400 * 30})
				}
				access.Slug = lookup
				access.Scope = digest("web:" + access.ChannelID + ":" + nonce)
				if action != "info" && (len(r.Header.Values("X-Visitor-Session")) != 1 || !hmac.Equal([]byte(r.Header.Get("X-Visitor-Session")), []byte(access.Scope))) {
					respond(w, 401, 40100, "Visitor session changed; reopen the application", nil)
					return
				}
			} else {
				access.Scope = digest("api:" + access.ChannelID + ":" + access.APIHash)
			}
			if action == "info" {
				var info map[string]any
				if json.Unmarshal(target.Info, &info) != nil {
					runHTTPError(w, ErrCapabilities)
					return
				}
				info["sessionId"] = access.Scope
				respond(w, 200, 0, "success", info)
				return
			}
			var body []byte
			if kind == "API" && action != "stream" {
				visitor := r.URL.Query().Get("visitorId")
				if len(visitor) > 128 || len(r.URL.Query()["visitorId"]) > 1 {
					respond(w, 400, 40000, "Invalid visitorId", nil)
					return
				}
				access.VisitorID = visitor
				if visitor != "" {
					access.Scope = digest("api:" + access.ChannelID + ":" + access.APIHash + ":" + visitor)
				}
			}
			if action == "stream" {
				var input struct {
					Message        string         `json:"message"`
					Inputs         map[string]any `json:"inputs"`
					ConversationID string         `json:"conversationId"`
					VisitorID      string         `json:"visitorId"`
				}
				if decode(w, r, &input) != nil || len(input.VisitorID) > 128 {
					respond(w, 400, 40000, "Invalid public request", nil)
					return
				}
				message := strings.TrimSpace(input.Message)
				if message == "" && len(input.Inputs) > 0 {
					data, _ := json.Marshal(input.Inputs)
					message = string(data)
				}
				// An API key is a service credential; optional visitorId subdivides its
				// conversations, not a second authentication factor.
				if kind == "API" && input.VisitorID != "" {
					access.VisitorID = input.VisitorID
					access.Scope = digest("api:" + access.ChannelID + ":" + access.APIHash + ":" + input.VisitorID)
				}
				body, _ = json.Marshal(RunSubmission{AgentID: access.AgentID, Message: message, ConversationID: input.ConversationID})
			}
			cloned := r.Clone(context.WithValue(r.Context(), publicKey{}, &access))
			cloned.Header = r.Header.Clone()
			cloned.Header.Del("Authorization")
			cloned = cloned.WithContext(context.WithValue(cloned.Context(), userKey{}, User{ID: access.OwnerID, Status: "active"}))
			cloned.URL.Path = "/api/agent-runs/" + r.PathValue("runId")
			cloned.URL.RawPath = ""
			if action == "stream" {
				cloned.URL.Path = "/api/agent-runs/stream"
				cloned.Body = http.NoBody
				if body != nil {
					cloned.Body = io.NopCloser(bytes.NewReader(body))
					cloned.ContentLength = int64(len(body))
				}
			}
			if action == "cancel" {
				cloned.URL.Path += "/cancel"
			}
			mux.ServeHTTP(w, cloned)
		}
	}
	mux.HandleFunc("GET /api/public/agents/{slug}", handler("WEB", "info"))
	mux.HandleFunc("POST /api/public/agents/{slug}/chat/stream", handler("WEB", "stream"))
	mux.HandleFunc("GET /api/public/agents/{slug}/runs/{runId}", handler("WEB", "query"))
	mux.HandleFunc("POST /api/public/agents/{slug}/runs/{runId}/cancel", handler("WEB", "cancel"))
	mux.HandleFunc("POST /api/public/agent-runs/stream", handler("API", "stream"))
	mux.HandleFunc("GET /api/public/agent-runs/{runId}", handler("API", "query"))
	mux.HandleFunc("POST /api/public/agent-runs/{runId}/cancel", handler("API", "cancel"))
	return nil
}
