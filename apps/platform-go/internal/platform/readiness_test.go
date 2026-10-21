package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadinessFailuresAndShutdown(t *testing.T) {
	lifecycle, stop := context.WithCancel(context.Background())
	defer stop()
	fail := false
	mux := http.NewServeMux()
	RegisterReadiness(mux, lifecycle, func(context.Context) error {
		if fail {
			return errors.New("private-database-secret")
		}
		return nil
	})
	probe := func(want int) {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		if w.Code != want || strings.Contains(w.Body.String(), "secret") || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	probe(200)
	fail = true
	probe(503)
	fail = false
	stop()
	probe(503)
}

func TestRuntimeReadinessAuthCompatibilityAndCancellation(t *testing.T) {
	const token = "runtime-readiness-test-token-32-characters"
	for _, tc := range []struct {
		name, body string
		status     int
		bad        bool
	}{
		{"ready", `{"status":"ready","tools_ready":true}`, 200, false},
		{"no gateway", `{"status":"ready","tools_ready":false}`, 200, true},
		{"old runtime", `{"status":"ok"}`, 200, true},
		{"wrong secret", `{}`, 401, true},
		{"redirect", `{}`, 302, true},
		{"malformed", `broken`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+token || r.URL.Path != "/internal/v1/ready" {
					t.Error("incorrect internal probe")
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			check, err := RuntimeReadiness(server.URL, token, true)
			if err != nil {
				t.Fatal(err)
			}
			if err = check(context.Background()); (err != nil) != tc.bad {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if check(ctx) == nil {
				t.Fatal("ignored canceled probe")
			}
		})
	}
}
