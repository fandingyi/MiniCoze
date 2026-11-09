package platform

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamAuthorizationBeforeUpstream(t *testing.T) {
	auth, store := setup(t)
	token, _ := auth.Sign(store.user)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: stream.done\ndata: {}\n\n")
	}))
	defer upstream.Close()
	mux := AuthHandler(auth)
	if err := RegisterStream(mux, auth, Access{Store: store}, upstream.URL); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, token, role, body string
		want                    int
	}{
		{"missing identity", "", "MEMBER", `{"agentId":"a1","message":"hello"}`, 401},
		{"cross workspace", token, "", `{"agentId":"a1","message":"hello"}`, 403},
		{"cross user conversation", token, "MEMBER", `{"agentId":"a1","message":"hello","conversationId":"c1"}`, 403},
		{"identity injection", token, "MEMBER", `{"agentId":"a1","message":"hello","userId":"u2"}`, 400},
		{"missing message", token, "MEMBER", `{"agentId":"a1"}`, 400},
		{"bad budget", token, "MEMBER", `{"agentId":"a1","message":"hello","maxTokens":0}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.role = tc.role
			store.conv.UserID = "u2"
			request := httptest.NewRequest("POST", "/api/agent-runs/stream", strings.NewReader(tc.body))
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer "+tc.token)
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("got %d: %s", response.Code, response.Body.String())
			}
			if calls.Load() != 0 {
				t.Fatal("denied request reached runtime")
			}
		})
	}
}

func TestStreamFlushAndCancel(t *testing.T) {
	auth, store := setup(t)
	token, _ := auth.Sign(store.user)
	cancelled := make(chan struct{})
	upstreamHeaders := make(chan http.Header, 1)
	frame := "id: r1:1\nevent: message.delta\ndata: {\"runId\":\"r1\",\"content\":\"你好🌏\",\"sequence\":1}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeaders <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frame)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	mux := AuthHandler(auth)
	if err := RegisterStream(mux, auth, Access{Store: store}, upstream.URL); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(mux)
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/api/agent-runs/stream", strings.NewReader(`{"agentId":"a1","message":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-User-Id", "u2")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal("first event was buffered", err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var received strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		received.WriteString(line)
		if line == "\n" {
			break
		}
	}
	if received.String() != frame {
		t.Fatalf("SSE altered: %s", received.String())
	}
	headers := <-upstreamHeaders
	if headers.Get("Authorization") != "Bearer "+token || headers.Get("X-User-Id") != "" {
		t.Fatal("identity forwarding incorrect")
	}
	cancel()
	response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request not cancelled")
	}
}

func TestStreamPreservesHTTPFailure(t *testing.T) {
	auth, store := setup(t)
	token, _ := auth.Sign(store.user)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { respond(w, 403, 40300, "denied", nil) }))
	defer upstream.Close()
	mux := AuthHandler(auth)
	if err := RegisterStream(mux, auth, Access{Store: store}, upstream.URL); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/api/agent-runs/stream", strings.NewReader(`{"agentId":"a1","message":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != 403 || strings.Contains(response.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("upstream failure lost: %d", response.Code)
	}
}

func TestCORS(t *testing.T) {
	handler := CORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), []string{"http://localhost:5173"})
	for _, tc := range []struct {
		origin string
		want   int
	}{{"http://localhost:5173", 204}, {"https://evil.example", 403}} {
		req := httptest.NewRequest("OPTIONS", "/api/auth/profile", nil)
		req.Header.Set("Origin", tc.origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tc.want {
			t.Fatal(response.Code)
		}
	}
}
