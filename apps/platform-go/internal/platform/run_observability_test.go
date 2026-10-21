package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type watchStore struct {
	RunStore
	err error
	run AgentRun
}

func (s watchStore) GetRun(context.Context, string, string) (AgentRun, error) { return s.run, s.err }
func TestRunWatchFailureAndRevocation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err, want error
	}{{"store unavailable", errors.New("secret database text"), errWatchUnavailable}, {"membership revoked", ErrNotFound, errAccessRevoked}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := make(chan struct{})
			go func() { watchRun(ctx, watchStore{err: tc.err}, "u", "r", cancel, 10*time.Millisecond); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("watch stuck")
			}
			if !errors.Is(context.Cause(ctx), tc.want) {
				t.Fatal(context.Cause(ctx))
			}
		})
	}
}

type failedFlushWriter struct{ *httptest.ResponseRecorder }

func (w failedFlushWriter) FlushError() error { return context.DeadlineExceeded }
func TestStreamPropagatesFlushFailure(t *testing.T) {
	w := &streamWriter{failedFlushWriter{httptest.NewRecorder()}}
	if err := writeGatewayEvent(w, runtimeEnvelope{1, "r:1", "r", 1, time.Now().UTC().Format(time.RFC3339Nano), "run.created", map[string]any{"conversationId": "c"}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("flush failure lost", err)
	}
}
func TestRoundReceiptValidation(t *testing.T) {
	state := eventState{runID: "r", phase: "running"}
	event := runtimeEnvelope{1, "r:1", "r", 1, time.Now().UTC().Format(time.RFC3339Nano), "model.round.completed", map[string]any{"round": float64(1), "durationMs": float64(1), "finishReason": "stop", "usage": nil}}
	if err := state.validate(event); err != nil {
		t.Fatal(err)
	}
	event.Sequence = 2
	event.EventID = "r:2"
	if err := state.validate(event); err == nil {
		t.Fatal("duplicate round accepted")
	}
	event.Payload["round"] = float64(2)
	event.Payload["usage"] = map[string]any{"inputTokens": -1, "outputTokens": 1, "totalTokens": 0}
	if err := state.validate(event); err == nil {
		t.Fatal("negative usage accepted")
	}
}

func testRunObservability(t *testing.T, p Postgres) {
	ctx := context.Background()
	start := func(key string) AgentRun {
		t.Helper()
		r, _, err := p.BeginRun(ctx, "u1", key, RunSubmission{AgentID: "a1", Message: "private prompt", Preview: true})
		if err != nil {
			t.Fatal(err)
		}
		r, err = p.StartRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := start("metrics-known")
	round := ModelRound{1, 10, "tool_calls", &TokenCounts{4, 3, 7}}
	for i := 0; i < 2; i++ {
		if err := p.RecordModelRound(ctx, r.ID, round); err != nil {
			t.Fatal(err)
		}
	}
	bad := round
	bad.DurationMS++
	if err := p.RecordModelRound(ctx, r.ID, bad); !errors.Is(err, ErrKeyConflict) {
		t.Fatal("round conflict", err)
	}
	if err := p.RecordModelRound(ctx, r.ID, ModelRound{2, 20, "stop", &TokenCounts{8, 2, 10}}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.FinishRun(ctx, r.ID, "COMPLETED", RunResult{Text: "private output"}); err != nil {
		t.Fatal(err)
	}
	metrics, err := p.RunMetrics(ctx, "u1", r.ID)
	if err != nil || !metrics.UsageComplete || metrics.ReportedUsage.Total != 17 || len(metrics.Rounds) != 2 {
		t.Fatal("usage aggregation", metrics, err)
	}
	if _, err = p.RunMetrics(ctx, "outsider", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("metrics leaked", err)
	}
	unknown := start("metrics-unknown")
	if err = p.RecordModelRound(ctx, unknown.ID, ModelRound{1, 3, "stop", nil}); err != nil {
		t.Fatal(err)
	}
	if _, err = p.FinishRun(ctx, unknown.ID, "COMPLETED", RunResult{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	metrics, err = p.RunMetrics(ctx, "u1", unknown.ID)
	if err != nil || metrics.UsageComplete || metrics.ReportedUsage != nil {
		t.Fatal("unknown converted to zero", metrics, err)
	}
	partial := start("metrics-partial")
	if err = p.RecordModelRound(ctx, partial.ID, round); err != nil {
		t.Fatal(err)
	}
	if _, err = p.FinishRun(ctx, partial.ID, "FAILED", RunResult{ErrorCode: "MODEL_TIMEOUT"}); err != nil {
		t.Fatal(err)
	}
	metrics, err = p.RunMetrics(ctx, "u1", partial.ID)
	if err != nil || metrics.UsageComplete || metrics.ReportedUsage.Total != 7 {
		t.Fatal("failed usage lost", metrics, err)
	}

	// Both sweeps must make progress even while another transaction owns a row.
	_, err = p.DB.Exec(`INSERT INTO "AgentRun"("id","conversationId","agentId","userId","workspaceId","requestKey","requestHash","mode","snapshot","history","inputText","status","createdAt","updatedAt","deadlineAt") VALUES('reaper-locked',$1,'a1','u1','w1','locked-key','hash','draft','{}','[]','x','CREATED',NOW(),NOW(),NOW()-interval '1 second')`, r.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT "id" FROM "AgentRun" WHERE "id"='reaper-locked' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	sweepCtx, stop := context.WithTimeout(ctx, time.Second)
	n, err := p.ExpireRuns(sweepCtx)
	stop()
	if err != nil || n != 0 {
		t.Fatal("reaper blocked on lock", n, err)
	}
	tx.Rollback()
	n, err = p.ExpireRuns(ctx)
	if err != nil || n != 1 {
		t.Fatal("reaper did not recover", n, err)
	}

	for _, scenario := range []string{"shutdown", "timeout", "remote-cancel", "disconnect"} {
		t.Run(scenario, func(t *testing.T) { testLifecycleHTTP(t, p, scenario) })
	}
}

func testLifecycleHTTP(t *testing.T, p Postgres, scenario string) {
	upstreamStarted, upstreamClosed := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command map[string]any
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			t.Error(err)
			return
		}
		limits := command["limits"].(map[string]any)
		if limits["deadline_ms"].(float64) > 110000 {
			t.Error("budget increased")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, frame(command["run_id"].(string), 1, "run.created", map[string]any{"conversationId": "ignored"}))
		w.(http.Flusher).Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamClosed)
	}))
	defer upstream.Close()
	auth, users := setup(t)
	token, _ := auth.Sign(users.user)
	mux := AuthHandler(auth)
	lifecycle, stopLifecycle := context.WithCancel(context.Background())
	defer stopLifecycle()
	var logs bytes.Buffer
	opts := DefaultRunOptions()
	opts.Lifecycle = lifecycle
	opts.PollInterval = 20 * time.Millisecond
	opts.MaxConcurrent = 1
	opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	if scenario == "timeout" {
		opts.Timeout = 400 * time.Millisecond
	}
	if err := RegisterPersistentRunsWithOptions(mux, auth, p, upstream.URL, "test-runtime-service-token-32-characters", opts); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(mux)
	defer gateway.Close()
	requestCtx, abort := context.WithCancel(context.Background())
	defer abort()
	req, _ := http.NewRequestWithContext(requestCtx, "POST", gateway.URL+"/api/agent-runs/stream", strings.NewReader(`{"agentId":"a1","preview":true,"message":"private prompt"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "lifecycle-"+scenario)
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	id := res.Header.Get("X-Run-Id")
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not start")
	}
	// Concurrent admission must fail without allocating a second run.
	second := httptest.NewRequest("POST", "/api/agent-runs/stream", strings.NewReader(`{"agentId":"a1","preview":true,"message":"private prompt"}`))
	second.Header.Set("Authorization", "Bearer "+token)
	second.Header.Set("Idempotency-Key", "extra-"+scenario)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, second)
	if rec.Code != 429 {
		t.Fatal("admission unbounded", rec.Code)
	}
	want := "RUN_TIMEOUT"
	switch scenario {
	case "shutdown":
		stopLifecycle()
		want = "SERVICE_SHUTDOWN"
	case "remote-cancel":
		_, err = p.FinishRun(context.Background(), id, "CANCELLED", RunResult{ErrorCode: "RUN_CANCELLED"})
		if err != nil {
			t.Fatal(err)
		}
		want = "RUN_CANCELLED"
	case "disconnect":
		abort()
		res.Body.Close()
		want = "CLIENT_DISCONNECTED"
	}
	body, _ := io.ReadAll(res.Body)
	select {
	case <-upstreamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream connection not cancelled")
	}
	var run AgentRun
	for i := 0; i < 100; i++ {
		run, err = p.GetRun(context.Background(), "u1", id)
		if err == nil && run.terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || !run.terminal() || run.ErrorCode == nil || *run.ErrorCode != want || run.OutputText != nil {
		t.Fatal("wrong terminal", run.Status, run.ErrorCode, err)
	}
	if scenario != "disconnect" && strings.Count(string(body), "event: run.failed\n") != 1 {
		t.Fatalf("terminal events %s", body)
	}
	// Stop the HTTP server before reading the shared logger buffer.
	gateway.Close()
	if strings.Contains(logs.String(), "private prompt") || strings.Contains(logs.String(), token) || !strings.Contains(logs.String(), id) {
		t.Fatal("logs leaked data or lost run identity")
	}
	if scenario == "shutdown" {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, second)
		if rec.Code != 503 {
			t.Fatal("accepted while draining")
		}
	}
}
