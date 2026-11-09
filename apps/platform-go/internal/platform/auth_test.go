package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type fakeStore struct {
	user    User
	err     error
	role    string
	agent   Agent
	conv    Conversation
	convErr error
	ensured int
}

func (s *fakeStore) UserByEmail(context.Context, string) (User, error) { return s.user, s.err }
func (s *fakeStore) UserByID(context.Context, string) (User, error)    { return s.user, s.err }
func (s *fakeStore) EnsureWorkspace(context.Context, string) error     { s.ensured++; return s.err }
func (s *fakeStore) WorkspaceRole(context.Context, string, string) (string, error) {
	if s.role == "" {
		return "", ErrNotFound
	}
	return s.role, s.err
}
func (s *fakeStore) Agent(context.Context, string) (Agent, error) { return s.agent, s.err }
func (s *fakeStore) Conversation(context.Context, string) (Conversation, error) {
	return s.conv, s.convErr
}
func setup(t *testing.T) (*Auth, *fakeStore) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{user: User{ID: "u1", Email: "user@example.com", Username: "user", PasswordHash: string(hash), Status: "active"}, role: "MEMBER", agent: Agent{ID: "a1", WorkspaceID: "w1", Status: "ACTIVE", HasVersion: true}, conv: Conversation{UserID: "u1", AgentID: "a1"}}
	auth, err := NewAuth(store, "test-secret-not-for-production", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return auth, store
}
func TestLoginAndProfile(t *testing.T) {
	auth, store := setup(t)
	handler := AuthHandler(auth)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"email":"user@example.com","password":"password123"}`)))
	if response.Code != 201 {
		t.Fatalf("login: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Data struct {
			AccessToken string `json:"accessToken"`
			TokenType   string `json:"tokenType"`
			User        User   `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Body.String(), "passwordHash") || body.Data.TokenType != "Bearer" || store.ensured != 1 {
		t.Fatal("incompatible login response")
	}
	claims, err := auth.Verify(body.Data.AccessToken)
	if err != nil || claims.Subject != "u1" || claims.Email != store.user.Email {
		t.Fatalf("claims: %+v %v", claims, err)
	}
	req := httptest.NewRequest("GET", "/api/auth/profile", nil)
	req.Header.Set("Authorization", "Bearer "+body.Data.AccessToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"id":"u1"`) {
		t.Fatal(response.Body.String())
	}
}
func TestLoginFailures(t *testing.T) {
	auth, store := setup(t)
	for _, tc := range []struct {
		name, body string
		err        error
		status     string
		want       int
	}{
		{"bad password", `{"email":"user@example.com","password":"wrong123"}`, nil, "active", 401},
		{"unknown user", `{"email":"user@example.com","password":"password123"}`, ErrNotFound, "active", 401},
		{"disabled", `{"email":"user@example.com","password":"password123"}`, nil, "disabled", 401},
		{"database down", `{"email":"user@example.com","password":"password123"}`, errors.New("database secret"), "active", 503},
		{"malformed", `{`, nil, "active", 400},
		{"unknown field", `{"email":"user@example.com","password":"password123","admin":true}`, nil, "active", 400},
		{"multiple values", `{"email":"user@example.com","password":"password123"}{}`, nil, "active", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.err = tc.err
			store.user.Status = tc.status
			response := httptest.NewRecorder()
			auth.Login(response, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(tc.body)))
			if response.Code != tc.want {
				t.Fatalf("got %d", response.Code)
			}
			if strings.Contains(response.Body.String(), "database secret") {
				t.Fatal("internal error leaked")
			}
		})
	}
}

func TestLegacyLongPassword(t *testing.T) {
	auth, store := setup(t)
	password := strings.Repeat("a", 80)
	hash, err := bcrypt.GenerateFromPassword([]byte(password[:72]), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store.user.PasswordHash = string(hash)
	body, _ := json.Marshal(map[string]string{"email": store.user.Email, "password": password})
	response := httptest.NewRecorder()
	auth.Login(response, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(string(body))))
	if response.Code != 201 {
		t.Fatalf("existing long-password account rejected: %d", response.Code)
	}
}
func TestTokenValidation(t *testing.T) {
	auth, _ := setup(t)
	for _, tc := range []struct {
		name   string
		method jwt.SigningMethod
		claims jwt.MapClaims
		key    any
	}{
		{"NestJS compatible", jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u1", "email": "user@example.com", "username": "user", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}, auth.secret},
		{"expired", jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u1", "exp": time.Now().Add(-time.Hour).Unix()}, auth.secret},
		{"missing expiration", jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u1"}, auth.secret},
		{"missing subject", jwt.SigningMethodHS256, jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()}, auth.secret},
		{"wrong key", jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()}, []byte("wrong")},
		{"wrong algorithm", jwt.SigningMethodHS384, jwt.MapClaims{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()}, auth.secret},
		{"unsigned", jwt.SigningMethodNone, jwt.MapClaims{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()}, jwt.UnsafeAllowNoneSignatureType},
		{"not active yet", jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(time.Minute).Unix()}, auth.secret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := jwt.NewWithClaims(tc.method, tc.claims).SignedString(tc.key)
			if err != nil {
				t.Fatal(err)
			}
			_, err = auth.Verify(raw)
			if (err == nil) != (tc.name == "NestJS compatible") {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
}
func TestMiddlewareRejectsInvalidIdentity(t *testing.T) {
	auth, store := setup(t)
	token, _ := auth.Sign(store.user)
	for _, tc := range []struct {
		name, header, status string
		err                  error
		want                 int
	}{
		{"missing", "", "active", nil, 401}, {"malformed", "Bearer broken", "active", nil, 401},
		{"deleted", "Bearer " + token, "active", ErrNotFound, 401}, {"disabled", "Bearer " + token, "disabled", nil, 401},
		{"database unavailable", "Bearer " + token, "active", errors.New("offline"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.user.Status = tc.status
			store.err = tc.err
			called := false
			handler := auth.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Authorization", tc.header)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if called || response.Code != tc.want {
				t.Fatalf("got %d called %v", response.Code, called)
			}
		})
	}
}
func TestAccessBoundaries(t *testing.T) {
	_, store := setup(t)
	access := Access{Store: store}
	ctx := context.Background()
	if err := access.Run(ctx, "u1", "a1", "c1", false); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"MEMBER", "ADMIN", "OWNER", ""} {
		store.role = role
		err := access.Workspace(ctx, "u1", "w1", true)
		if (err == nil) != (role == "ADMIN" || role == "OWNER") {
			t.Fatalf("manage role %s: %v", role, err)
		}
	}
	store.role = ""
	if err := access.Run(ctx, "u1", "a1", "c1", false); !errors.Is(err, ErrForbidden) {
		t.Fatal("cross-workspace allowed")
	}
	store.role = "MEMBER"
	for _, conv := range []Conversation{{UserID: "u2", AgentID: "a1"}, {UserID: "u1", AgentID: "a2"}, {UserID: "u1", AgentID: "a1", Preview: true}} {
		store.conv = conv
		if err := access.Run(ctx, "u1", "a1", "c1", false); !errors.Is(err, ErrForbidden) {
			t.Fatal("conversation boundary bypassed")
		}
	}
	store.agent.Status = "DRAFT"
	if err := access.Run(ctx, "u1", "a1", "", false); !errors.Is(err, ErrUnpublished) {
		t.Fatal("unpublished agent allowed")
	}
	if err := access.Run(ctx, "u1", "a1", "", true); err != nil {
		t.Fatal("preview denied", err)
	}
}
