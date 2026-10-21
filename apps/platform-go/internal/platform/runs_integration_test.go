package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This database is disposable and never reads the application's DATABASE_URL.
func TestPostgresRunLifecycle(t *testing.T) {
	if os.Getenv("RUN_POSTGRES_INTEGRATION") != "1" {
		t.Skip("set RUN_POSTGRES_INTEGRATION=1 for isolated PostgreSQL")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	root := t.TempDir()
	config := embeddedpostgres.DefaultConfig().Port(uint32(port)).Username("run_test").Password("local_test_only").Database("run_test").RuntimePath(filepath.Join(root, "runtime")).DataPath(filepath.Join(root, "data")).CachePath(filepath.Join(os.TempDir(), "minicoze-pg-test-cache")).BinaryRepositoryURL("https://repo.maven.apache.org/maven2").StartTimeout(60 * time.Second).Logger(io.Discard)
	server := embeddedpostgres.NewDatabase(config)
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	dsn, err := DatabaseURL(config.GetConnectionURL())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE "User"("id" text PRIMARY KEY,"status" text); INSERT INTO "User" VALUES('u1','active');
 CREATE TABLE "WorkspaceMember"("workspaceId" text,"userId" text,PRIMARY KEY("workspaceId","userId"));
 CREATE TABLE "Agent"("id" text PRIMARY KEY,"workspaceId" text,"model" text,"temperature" double precision,"systemPrompt" text,"contextLimit" int,"status" text,"currentVersionId" text);
 CREATE TABLE "AgentVersion"("id" text PRIMARY KEY,"agentId" text,"snapshot" jsonb);
 CREATE TABLE "AgentPluginBinding"("agentId" text); CREATE TABLE "KnowledgeBaseAgentBinding"("agentId" text); CREATE TABLE "AgentWorkflow"("agentId" text);
 CREATE TABLE "Conversation"("id" text PRIMARY KEY,"agentId" text,"userId" text,"title" text,"isPreview" boolean,"createdAt" timestamp(3),"updatedAt" timestamp(3));
 CREATE TABLE "Message"("id" text PRIMARY KEY,"conversationId" text,"role" text,"content" text,"model" text,"tokenUsage" jsonb,"createdAt" timestamp(3));
 INSERT INTO "WorkspaceMember" VALUES('w1','u1'); INSERT INTO "Agent" VALUES('a1','w1','test-model',0.7,'original',10,'ACTIVE',NULL);`)
	migration, err := os.ReadFile("../../../backend/prisma/migrations/20261014090000_add_persistent_agent_runs/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(migration))
	migration, err = os.ReadFile("../../../backend/prisma/migrations/20261016090000_add_agent_tool_calls/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(migration))
	migration, err = os.ReadFile("../../../backend/prisma/migrations/20261016093000_add_model_round_metrics/migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(migration))
	store := Postgres{DB: db}
	ctx := context.Background()
	input := RunSubmission{AgentID: "a1", Message: "hello", Preview: true}
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, _, err := store.BeginRun(ctx, "u1", "same-key", input)
			ids <- run.ID
			errs <- err
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for value := range ids {
		if id != "" && id != value {
			t.Fatal("duplicate run")
		}
		id = value
	}
	changed := input
	changed.Message = "different"
	if _, _, err = store.BeginRun(ctx, "u1", "same-key", changed); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("conflict: %v", err)
	}
	run, err := store.GetRun(ctx, "u1", id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetRun(ctx, "outsider", id); !errors.Is(err, ErrNotFound) {
		t.Fatal("owner isolation", err)
	}
	exec(`UPDATE "Agent" SET "systemPrompt"='edited' WHERE "id"='a1'`)
	run, err = store.StartRun(ctx, id)
	if err != nil || run.Snapshot.Instructions != "original" {
		t.Fatal("snapshot changed", err)
	}
	next := input
	next.ConversationID = run.ConversationID
	if _, _, err = store.BeginRun(ctx, "u1", "busy-key", next); !errors.Is(err, ErrRunBusy) {
		t.Fatal("busy", err)
	}
	exec(`ALTER TABLE "Message" ADD CONSTRAINT test_failure CHECK("content" <> 'reject')`)
	if _, err = store.FinishRun(ctx, id, "COMPLETED", RunResult{Text: "reject"}); err == nil {
		t.Fatal("expected transaction failure")
	}
	run, err = store.GetRun(ctx, "u1", id)
	if err != nil || run.Status != "RUNNING" || run.OutputText != nil {
		t.Fatal("partial commit", err)
	}
	run, err = store.FinishRun(ctx, id, "COMPLETED", RunResult{Text: "answer"})
	if err != nil || run.OutputText == nil {
		t.Fatal("finish", err)
	}
	if _, err = store.FinishRun(ctx, id, "CANCELLED", RunResult{}); err != nil {
		t.Fatal(err)
	}
	run, _, err = store.BeginRun(ctx, "u1", "next-key", next)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.History) != 2 || run.History[0].Role != "user" || run.History[1].Content != "answer" {
		t.Fatalf("history: %+v", run.History)
	}
	if _, err = store.StartRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"COMPLETED", "CANCELLED"} {
		wg.Add(1)
		go func(status string) {
			defer wg.Done()
			_, err := store.FinishRun(ctx, run.ID, status, RunResult{Text: "race"})
			if err != nil {
				t.Error(err)
			}
		}(status)
	}
	wg.Wait()
	final, err := store.GetRun(ctx, "u1", run.ID)
	if err != nil || !final.terminal() {
		t.Fatal("terminal race", err)
	}
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM "Message" WHERE "runId"=$1 AND "role"='ASSISTANT'`, run.ID).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if (final.Status == "COMPLETED" && count != 1) || (final.Status == "CANCELLED" && count != 0) {
		t.Fatal("inconsistent terminal/message")
	}
	if _, err = db.Exec(`UPDATE "AgentRun" SET "snapshot"='{}' WHERE "id"=$1`, id); err == nil {
		t.Fatal("snapshot mutable")
	}
	if _, err = db.Exec(`UPDATE "AgentRun" SET "status"='FAILED' WHERE "id"=$1`, id); err == nil {
		t.Fatal("terminal mutable")
	}
	// An interrupted process leaves a row for the bounded recovery sweep.
	exec(`INSERT INTO "AgentRun"("id","conversationId","agentId","userId","workspaceId","requestKey","requestHash","mode","snapshot","history","inputText","status","createdAt","updatedAt","deadlineAt") VALUES('expired',$1,'a1','u1','w1','expired-key','hash','draft','{}','[]','x','CREATED',NOW(),NOW(),NOW()-interval '1 second')`, run.ConversationID)
	n, err := store.ExpireRuns(ctx)
	if err != nil || n != 1 {
		t.Fatal("expiry", n, err)
	}
	t.Log(fmt.Sprintf("real PostgreSQL verified: replay, ownership, immutable snapshots, rollback, history, terminal race (%s), expiry", final.Status))
	t.Run("persistent HTTP", func(t *testing.T) { testPersistentHTTP(t, store) })
	t.Run("lifecycle and metrics", func(t *testing.T) { testRunObservability(t, store) })
	t.Run("tool gateway", func(t *testing.T) { testToolGateway(t, store) })
	t.Run("published version", func(t *testing.T) {
		exec(`INSERT INTO "AgentVersion" VALUES('v1','a1','{"agent":{"id":"a1","workspaceId":"w1","model":"published-model","systemPrompt":"published prompt","temperature":0.5,"contextLimit":4},"plugins":[],"workflows":[],"channels":[]}'); UPDATE "Agent" SET "currentVersionId"='v1' WHERE "id"='a1'`)
		forged := "forged-model"
		published, _, err := store.BeginRun(ctx, "u1", "published-key", RunSubmission{AgentID: "a1", Message: "published", Model: &forged})
		if err != nil {
			t.Fatal(err)
		}
		if published.Snapshot.Model.Name != "published-model" || published.AgentVersionID == nil {
			t.Fatal("published snapshot not respected")
		}
		_, err = store.FinishRun(ctx, published.ID, "CANCELLED", RunResult{})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("published public acceptance", func(t *testing.T) { testPublishedPublic(t, store) })
}

func testPersistentHTTP(t *testing.T, store Postgres) {
	auth, users := setup(t)
	token, _ := auth.Sign(users.user)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var payload struct {
			RunID string `json:"run_id"`
			Input struct {
				Text string `json:"text"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-runtime-service-token-32-characters" {
			t.Error("internal identity")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if payload.Input.Text == "wait" {
			fmt.Fprint(w, frame(payload.RunID, 1, "run.created", map[string]any{"conversationId": "ignored"}))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, normalStream(payload.RunID))
	}))
	defer upstream.Close()
	mux := AuthHandler(auth)
	if err := RegisterPersistentRuns(mux, auth, store, upstream.URL, "test-runtime-service-token-32-characters"); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	body := `{"agentId":"a1","preview":true,"message":"http"}`
	if w := request("POST", "/api/agent-runs/stream", "", body); w.Code != 400 {
		t.Fatal("missing key")
	}
	first := request("POST", "/api/agent-runs/stream", "http-key", body)
	if first.Code != 200 || strings.Count(first.Body.String(), "event: run.completed\n") != 1 {
		t.Fatalf("stream: %d %s", first.Code, first.Body.String())
	}
	id := first.Header().Get("X-Run-Id")
	run, err := store.GetRun(context.Background(), "u1", id)
	if err != nil || run.Status != "COMPLETED" || run.OutputText == nil {
		t.Fatal("not committed", err)
	}
	replay := request("POST", "/api/agent-runs/stream", "http-key", body)
	if calls.Load() != 1 || replay.Header().Get("Idempotency-Replayed") != "true" || !strings.Contains(replay.Body.String(), "你好🌏") {
		t.Fatal("replay invoked model or lost result")
	}
	if w := request("GET", "/api/agent-runs/"+id, "", ""); w.Code != 200 {
		t.Fatal("query", w.Code)
	}
	if w := request("POST", "/api/agent-runs/"+id+"/cancel", "", ""); !strings.Contains(w.Body.String(), "COMPLETED") {
		t.Fatal("terminal replaced")
	}
	gateway := httptest.NewServer(mux)
	defer gateway.Close()
	req, _ := http.NewRequest("POST", gateway.URL+"/api/agent-runs/stream", strings.NewReader(`{"agentId":"a1","preview":true,"message":"wait"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "cancel-key")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	id = resp.Header.Get("X-Run-Id")
	if w := request("POST", "/api/agent-runs/"+id+"/cancel", "", ""); w.Code != 200 {
		t.Fatal("cancel", w.Code)
	}
	output, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.GetRun(context.Background(), "u1", id)
	if err != nil || run.Status != "CANCELLED" || run.OutputText != nil {
		t.Fatal("cancel persistence", err)
	}
	if strings.Count(string(output), "event: run.failed\n") != 1 || strings.Contains(string(output), "event: run.completed\n") {
		t.Fatal("cancel terminal stream", string(output))
	}
}
