package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSnapshots struct {
	snapshot PreviewSnapshot
	err      error
	user     string
}

func (f *fakeSnapshots) PreviewSnapshot(_ context.Context, user, agent string) (PreviewSnapshot, error) {
	f.user = user
	return f.snapshot, f.err
}
func previewFixture() *fakeSnapshots {
	f := &fakeSnapshots{snapshot: PreviewSnapshot{AgentID: "a1", WorkspaceID: "w1", SchemaVersion: 1, Instructions: "immutable test instructions", Tools: []any{}, Knowledge: []any{}}}
	f.snapshot.Model.Name = "test-model"
	f.snapshot.Model.Temperature = 0.7
	return f
}
func frame(run string, seq int, kind string, payload map[string]any) string {
	data, _ := json.Marshal(runtimeEnvelope{1, fmt.Sprintf("%s:%d", run, seq), run, seq, time.Now().UTC().Format(time.RFC3339Nano), kind, payload})
	return fmt.Sprintf("event: %s\ndata: %s\n\n", kind, data)
}
func normalStream(run string) string {
	return frame(run, 1, "run.created", map[string]any{"conversationId": "preview-" + run}) + frame(run, 2, "run.in_progress", map[string]any{}) + frame(run, 3, "message.delta", map[string]any{"messageId": "m1", "content": "你好🌏"}) + frame(run, 4, "message.completed", map[string]any{"messageId": "m1", "content": "你好🌏"}) + frame(run, 5, "run.completed", map[string]any{}) + frame(run, 6, "stream.done", map[string]any{})
}

func TestGatewayRejectsInvalidInternalEvents(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
		valid        bool
	}{
		{"valid", normalStream("r1"), true},
		{"wrong run", normalStream("other"), false},
		{"skipped sequence", frame("r1", 2, "run.created", map[string]any{"conversationId": "p"}), false},
		{"premature completion", frame("r1", 1, "run.completed", map[string]any{}), false},
		{"truncated", frame("r1", 1, "run.created", map[string]any{"conversationId": "p"}), false},
		{"final mismatch", frame("r1", 1, "run.created", map[string]any{"conversationId": "p"}) + frame("r1", 2, "run.in_progress", map[string]any{}) + frame("r1", 3, "message.completed", map[string]any{"messageId": "m", "content": "not emitted"}), false},
		{"invalid JSON", "data: {broken}\n\n", false},
		{"unknown payload field", frame("r1", 1, "run.created", map[string]any{"conversationId": "p", "secret": "must not escape"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := eventState{runID: "r1"}
			response := httptest.NewRecorder()
			err := forwardRuntime(strings.NewReader(tc.stream), &streamWriter{response}, &state)
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected validation %v", err)
			}
		})
	}
}

func TestPythonGatewayIdentitySnapshotAndFailure(t *testing.T) {
	auth, users := setup(t)
	token, _ := auth.Sign(users.user)
	snapshots := previewFixture()
	const serviceToken = "test-runtime-service-token-32-characters"
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+serviceToken {
			t.Error("wrong internal credential")
		}
		var command map[string]any
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			t.Error(err)
		}
		if command["actor"].(map[string]any)["user_id"] != "u1" {
			t.Error("wrong actor")
		}
		if command["snapshot"].(map[string]any)["instructions"] != "immutable test instructions" {
			t.Error("snapshot lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, normalStream(command["run_id"].(string)))
	}))
	defer upstream.Close()
	mux := AuthHandler(auth)
	if err := RegisterPythonPreview(mux, auth, snapshots, upstream.URL, serviceToken); err != nil {
		t.Fatal(err)
	}
	invoke := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/agent-runs/preview/stream", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		return res
	}
	response := invoke(`{"agentId":"a1","message":"hello"}`)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"content":"你好🌏"`) || response.Header().Get("X-Run-Persistence") != "none" {
		t.Fatal(response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"payload"`) || strings.Contains(response.Body.String(), serviceToken) {
		t.Fatal("internal envelope or secret leaked")
	}
	snapshots.err = ErrNotFound
	if invoke(`{"agentId":"a1","message":"hello"}`).Code != 404 {
		t.Fatal("missing Agent allowed")
	}
	snapshots.err = nil
	snapshots.snapshot.HasCapabilities = true
	if invoke(`{"agentId":"a1","message":"hello"}`).Code != 409 {
		t.Fatal("unsupported capabilities ignored")
	}
	if invoke(`{"agentId":"a1","message":"hello","userId":"u2"}`).Code != 400 {
		t.Fatal("identity injection accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("denied request reached Python")
	}
}

func TestGatewayTruncationProducesSingleFailure(t *testing.T) {
	auth, users := setup(t)
	token, _ := auth.Sign(users.user)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command struct {
			RunID string `json:"run_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&command)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frame(command.RunID, 1, "run.created", map[string]any{"conversationId": "p"}))
	}))
	defer upstream.Close()
	mux := AuthHandler(auth)
	if err := RegisterPythonPreview(mux, auth, previewFixture(), upstream.URL, "test-runtime-service-token-32-characters"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/agent-runs/preview/stream", strings.NewReader(`{"agentId":"a1","message":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if strings.Count(res.Body.String(), "event: run.failed\n") != 1 || strings.Contains(res.Body.String(), "event: run.completed\n") || !strings.Contains(res.Body.String(), "event: stream.done\n") {
		t.Fatal(res.Body.String())
	}
}
