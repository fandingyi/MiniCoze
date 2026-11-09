package platform

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

var ErrNotFound = errors.New("not found")

type User struct {
	ID           string  `json:"id"`
	Username     string  `json:"username"`
	Email        string  `json:"email"`
	PasswordHash string  `json:"-"`
	Phone        string  `json:"phone"`
	Bio          string  `json:"bio"`
	AvatarURL    *string `json:"avatarUrl"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    string  `json:"updatedAt"`
}

type Store interface {
	UserByEmail(context.Context, string) (User, error)
	UserByID(context.Context, string) (User, error)
	EnsureWorkspace(context.Context, string) error
	WorkspaceRole(context.Context, string, string) (string, error)
	Agent(context.Context, string) (Agent, error)
	Conversation(context.Context, string) (Conversation, error)
}

type Claims struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

type Auth struct {
	store      Store
	secret     []byte
	ttl        time.Duration
	dummyHash  []byte
	loginSlots chan struct{}
}

func NewAuth(store Store, secret string, ttl time.Duration) (*Auth, error) {
	if strings.TrimSpace(secret) == "" || ttl <= 0 {
		return nil, errors.New("JWT_SECRET and positive JWT_EXPIRES_IN required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("invalid-password-sentinel"), 10)
	if err != nil {
		return nil, err
	}
	return &Auth{store: store, secret: []byte(secret), ttl: ttl, dummyHash: hash, loginSlots: make(chan struct{}, 8)}, nil
}

func (a *Auth) Sign(u User) (string, error) {
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Email: u.Email, Username: u.Username,
		RegisteredClaims: jwt.RegisteredClaims{Subject: u.ID, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(a.ttl))},
	}).SignedString(a.secret)
}

func (a *Auth) Verify(raw string) (Claims, error) {
	var claims Claims
	token, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) { return a.secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || token == nil || !token.Valid || claims.Subject == "" {
		return Claims{}, errors.New("invalid token")
	}
	return claims, nil
}

type userKey struct{}

func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields := strings.Fields(r.Header.Get("Authorization"))
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || len(r.Header.Values("Authorization")) != 1 {
			respond(w, 401, 40100, "未登录或 Token 无效", nil)
			return
		}
		claims, err := a.Verify(fields[1])
		if err != nil {
			respond(w, 401, 40100, "未登录或 Token 无效", nil)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		u, err := a.store.UserByID(ctx, claims.Subject)
		cancel()
		if errors.Is(err, ErrNotFound) || (err == nil && u.Status != "active") {
			respond(w, 401, 40100, "用户不存在或已停用", nil)
			return
		}
		if err != nil {
			respond(w, 503, 50000, "身份服务暂不可用", nil)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	select {
	case a.loginSlots <- struct{}{}:
		defer func() { <-a.loginSlots }()
	default:
		respond(w, 429, 42900, "登录请求繁忙，请稍后重试", nil)
		return
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if decode(w, r, &input) != nil || !validEmail(input.Email) || len([]rune(input.Password)) < 6 {
		respond(w, 400, 40000, "邮箱或密码格式错误", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	u, err := a.store.UserByEmail(ctx, input.Email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		respond(w, 503, 50000, "身份服务暂不可用", nil)
		return
	}
	hash := []byte(u.PasswordHash)
	if errors.Is(err, ErrNotFound) {
		hash = a.dummyHash
	}
	// Node bcrypt used by existing accounts truncates passwords at 72 bytes.
	password := []byte(input.Password)
	if len(password) > 72 {
		password = password[:72]
	}
	passwordErr := bcrypt.CompareHashAndPassword(hash, password)
	if err != nil || passwordErr != nil || u.Status != "active" {
		respond(w, 401, 10002, "邮箱或密码错误", nil)
		return
	}
	if err = a.store.EnsureWorkspace(ctx, u.ID); err != nil {
		respond(w, 503, 50000, "工作空间初始化失败", nil)
		return
	}
	token, err := a.Sign(u)
	if err != nil {
		respond(w, 500, 50000, "登录失败", nil)
		return
	}
	respond(w, 201, 0, "success", map[string]any{"accessToken": token, "tokenType": "Bearer", "user": u})
}

func (a *Auth) Profile(w http.ResponseWriter, r *http.Request) {
	respond(w, 200, 0, "success", r.Context().Value(userKey{}))
}

// Public delegation is in-process only and restricted to run routes. Normal
// identity endpoints never accept this principal instead of a user JWT.
func (a *Auth) RunMiddleware(next http.Handler) http.Handler {
	private := a.Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if access := publicAccess(r.Context()); access != nil {
			user, ok := r.Context().Value(userKey{}).(User)
			if !ok || user.ID != access.OwnerID {
				respond(w, 401, 40100, "Invalid public identity", nil)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		private.ServeHTTP(w, r)
	})
}
