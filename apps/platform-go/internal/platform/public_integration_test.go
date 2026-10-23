package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testPublishedPublic(t *testing.T, store Postgres) {
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`ALTER TABLE "Agent" ADD COLUMN "creatorId" text DEFAULT 'u1'; CREATE TABLE "PublishChannel"("id" text PRIMARY KEY,"workspaceId" text,"targetId" text,"targetType" text,"channel" text,"enabled" boolean,"config" jsonb)`)
	snapshot := `{"agent":{"id":"tool-agent","workspaceId":"w1","name":"Published demo","model":"test-model","systemPrompt":"frozen prompt","temperature":0.5,"contextLimit":4},"plugins":[{"pluginId":"plugin","pluginCode":"system_tools","status":"ACTIVE","autoInvoke":true,"config":{},"tools":[{"type":"function","function":{"name":"system_tools__echo_text"}}]}],"knowledge":[{"knowledgeBaseId":"kb","name":"Docs","enabled":true}],"workflows":[],"channels":[]}`
	exec(`INSERT INTO "AgentVersion" VALUES('v7','tool-agent',$1)`, snapshot)
	exec(`UPDATE "Agent" SET "currentVersionId"='v7',"systemPrompt"='changed draft',"model"='changed-model' WHERE "id"='tool-agent'`)
	saved, _, err := store.BeginRun(ctx, "u1", "published-tools-check", RunSubmission{AgentID: "tool-agent", Message: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Snapshot.Instructions != "frozen prompt" || saved.Snapshot.Model.Name != "test-model" || len(saved.Snapshot.Capabilities) != 2 {
		t.Fatalf("publication isolation: %+v", saved.Snapshot)
	}
	for _, cap := range saved.Snapshot.Capabilities {
		if cap.Code == "time_now" {
			t.Fatal("draft tool leaked")
		}
	}
	if _, err = store.FinishRun(ctx, saved.ID, "CANCELLED", RunResult{}); err != nil {
		t.Fatal(err)
	}
	t.Run("published Python tools", func(t *testing.T) { testRealToolChain(t, store, true) })
	exec(`INSERT INTO "PublishChannel" VALUES('web7','w1','tool-agent','AGENT','WEB',true,'{"slug":"demo"}'),('api7','w1','tool-agent','AGENT','API',true,$1)`, fmt.Sprintf(`{"apiKeyHash":%q}`, digest("test-api-key-day-seven")))
	auth, users := setup(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct {
			RunID string `json:"run_id"`
			Input struct {
				Text string `json:"text"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if input.Input.Text == "api-wait" {
			fmt.Fprint(w, frame(input.RunID, 1, "run.created", map[string]any{"conversationId": "ignored"}))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, normalStream(input.RunID))
	}))
	defer upstream.Close()
	mux := AuthHandler(auth)
	options := DefaultRunOptions()
	options.MaxPerChannel = 1
	if err := RegisterPersistentRunsWithOptions(mux, auth, store, upstream.URL, "test-runtime-service-token-32-characters", options); err != nil {
		t.Fatal(err)
	}
	if err := RegisterPublicRoutes(mux, store, "test-public-session-secret-32-characters", false); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, key, body string, cookie *http.Cookie, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if cookie != nil {
			r.AddCookie(cookie)
			r.Header.Set("X-Visitor-Session", digest("web:web7:"+strings.Split(cookie.Value, ".")[0]))
		}
		r.Header.Set("Idempotency-Key", key)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	session := func() *http.Cookie {
		t.Helper()
		w := request("GET", "/api/public/agents/demo", "", "", nil, "")
		cookies := w.Result().Cookies()
		if w.Code != 200 || len(cookies) != 1 || !cookies[0].HttpOnly || strings.Contains(w.Body.String(), "frozen prompt") {
			t.Fatalf("info/session: %d %s", w.Code, w.Body.String())
		}
		return cookies[0]
	}
	a, b := session(), session()
	path := "/api/public/agents/demo/chat/stream"
	first := request("POST", path, "public-same-key", `{"message":"hello"}`, a, "")
	if first.Code != 200 || !strings.Contains(first.Body.String(), "event: run.completed") {
		t.Fatalf("public: %d %s", first.Code, first.Body.String())
	}
	id := first.Header().Get("X-Run-Id")
	run, err := store.GetRun(ctx, "u1", id)
	if err != nil {
		t.Fatal(err)
	}
	replay := request("POST", path, "public-same-key", `{"message":"hello"}`, a, "")
	if replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
		t.Fatal("public replay duplicated model")
	}
	other := request("POST", path, "public-same-key", `{"message":"hello"}`, b, "")
	if other.Code != 200 || other.Header().Get("X-Run-Id") == id {
		t.Fatal("visitor key isolation")
	}
	body := fmt.Sprintf(`{"message":"steal","conversationId":%q,"visitorId":"forged"}`, run.ConversationID)
	if w := request("POST", path, "cross-visitor-key", body, b, ""); w.Code != 404 {
		t.Fatalf("cross conversation: %d %s", w.Code, w.Body.String())
	}
	query := "/api/public/agents/demo/runs/" + id
	if w := request("GET", query, "", "", a, ""); w.Code != 200 {
		t.Fatal("owner query", w.Code, w.Body.String())
	}
	for _, method := range []string{"GET", "POST"} {
		p := query
		if method == "POST" {
			p += "/cancel"
		}
		if w := request(method, p, "", "", b, ""); w.Code != 404 {
			t.Fatal("cross visitor run access", w.Code)
		}
	}
	tampered := *a
	tampered.Value += "x"
	if w := request("GET", query, "", "", &tampered, ""); w.Code != 401 {
		t.Fatal("tampered cookie", w.Code)
	}
	if w := request("POST", path, "override-key", `{"message":"hello","preview":true}`, a, ""); w.Code != 400 {
		t.Fatal("public override accepted", w.Code)
	}
	apiPath := "/api/public/agent-runs/stream"
	beforeLostSession := calls.Load()
	if w := request("POST", path, "public-same-key", `{"message":"hello"}`, nil, ""); w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Fatal("missing session recreated on retry", w.Code)
	}
	stale := httptest.NewRequest("POST", path, strings.NewReader(`{"message":"hello"}`))
	stale.AddCookie(b)
	stale.Header.Set("Idempotency-Key", "public-same-key")
	stale.Header.Set("X-Visitor-Session", digest("web:web7:"+strings.Split(a.Value, ".")[0]))
	staleResponse := httptest.NewRecorder()
	mux.ServeHTTP(staleResponse, stale)
	if staleResponse.Code != 401 || calls.Load() != beforeLostSession {
		t.Fatal("changed session repeated execution")
	}
	apiRun := request("POST", apiPath, "api-visitor-lifecycle", `{"message":"api lifecycle","visitorId":"customer-a"}`, nil, "test-api-key-day-seven")
	apiID := apiRun.Header().Get("X-Run-Id")
	if apiRun.Code != 200 || !strings.HasSuffix(apiRun.Header().Get("Location"), "?visitorId=customer-a") {
		t.Fatal("API lifecycle creation", apiRun.Code, apiRun.Body.String())
	}
	for _, method := range []string{"GET", "POST"} {
		path := "/api/public/agent-runs/" + apiID
		if method == "POST" {
			path += "/cancel"
		}
		if w := request(method, path+"?visitorId=customer-a", "", "", nil, "test-api-key-day-seven"); w.Code != 200 || !strings.Contains(w.Body.String(), "COMPLETED") {
			t.Fatal("API owner lifecycle", w.Code, w.Body.String())
		}
		for _, visitor := range []string{"", "?visitorId=customer-b"} {
			if w := request(method, path+visitor, "", "", nil, "test-api-key-day-seven"); w.Code != 404 {
				t.Fatal("API namespace isolation", w.Code)
			}
		}
		if w := request(method, path+"?visitorId=customer-a", "", "", nil, ""); w.Code != 401 {
			t.Fatal("API lifecycle authentication", w.Code)
		}
	}
	if w := request("POST", apiPath, "public-same-key", `{"message":"api"}`, nil, "test-api-key-day-seven"); w.Code != 200 {
		t.Fatal("api", w.Code, w.Body.String())
	}
	gateway := httptest.NewServer(mux)
	client := &http.Client{Timeout: 5 * time.Second}
	activeRequest, _ := http.NewRequest("POST", gateway.URL+apiPath, strings.NewReader(`{"message":"api-wait","visitorId":"customer-a"}`))
	activeRequest.Header.Set("Authorization", "Bearer test-api-key-day-seven")
	activeRequest.Header.Set("Idempotency-Key", "api-active-cancel")
	activeResponse, activeError := client.Do(activeRequest)
	if activeError != nil {
		gateway.Close()
		t.Fatal(activeError)
	}
	activeID := activeResponse.Header.Get("X-Run-Id")
	if w := request("POST", apiPath, "limited-channel-request", `{"message":"limited"}`, nil, "test-api-key-day-seven"); w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("per-channel admission", w.Code)
	}
	if w := request("POST", path, "other-channel-progress", `{"message":"other channel"}`, a, ""); w.Code != 200 {
		t.Fatal("other channel starved", w.Code)
	}
	if w := request("GET", "/api/public/agent-runs/"+activeID+"?visitorId=customer-a", "", "", nil, "test-api-key-day-seven"); w.Code != 200 {
		t.Fatal("query blocked by admission", w.Code)
	}
	if w := request("POST", "/api/public/agent-runs/"+activeID+"/cancel?visitorId=customer-a", "", "", nil, "test-api-key-day-seven"); w.Code != 200 {
		t.Fatal("active API cancel", w.Code, w.Body.String())
	}
	streamOutput, readError := io.ReadAll(activeResponse.Body)
	activeResponse.Body.Close()
	gateway.Close()
	if readError != nil || !strings.Contains(string(streamOutput), "event: run.failed") {
		t.Fatal("API cancellation did not reach stream", readError, string(streamOutput))
	}
	if saved, err := store.GetRun(ctx, "u1", activeID); err != nil || saved.Status != "CANCELLED" {
		t.Fatal("API cancel not persisted", err)
	}
	for _, expiry := range []string{"2000-01-01T00:00:00Z", "invalid"} {
		exec(`UPDATE "PublishChannel" SET "config"=jsonb_set("config",'{expiresAt}',to_jsonb($1::text)) WHERE "id"='api7'`, expiry)
		if w := request("POST", apiPath, "expired-api-key", `{"message":"api"}`, nil, "test-api-key-day-seven"); w.Code != 401 {
			t.Fatal("expiry", w.Code)
		}
	}
	exec(`UPDATE "PublishChannel" SET "config"=$1 WHERE "id"='api7'`, fmt.Sprintf(`{"apiKeyHash":%q}`, digest("rotated-api-key-day-seven")))
	if w := request("POST", apiPath, "rotated-api-key", `{"message":"api"}`, nil, "test-api-key-day-seven"); w.Code != 401 {
		t.Fatal("rotation", w.Code)
	}
	exec(`UPDATE "PublishChannel" SET "enabled"=false WHERE "id"='web7'`)
	if w := request("GET", query, "", "", a, ""); w.Code == 200 {
		t.Fatal("disabled channel query")
	}
	exec(`UPDATE "PublishChannel" SET "enabled"=true WHERE "id"='web7'`)
	// A deterministic local gateway/database baseline, not a model performance claim.
	timings := make([]time.Duration, 0, 16)
	for i := 0; i < 16; i++ {
		start := time.Now()
		w := request("POST", path, fmt.Sprintf("baseline-%03d", i), `{"message":"baseline"}`, a, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "event: run.completed") {
			t.Fatal("baseline failure", w.Body.String())
		}
		timings = append(timings, time.Since(start))
	}
	sort.Slice(timings, func(i, j int) bool { return timings[i] < timings[j] })
	t.Logf("local gateway + disposable PostgreSQL + mock runtime: n=16 concurrency=1 success=16 p50=%s p95=%s", timings[7], timings[15])
	// Rehearse routing rollback against a simulated NestJS SSE endpoint.
	var legacyCalls atomic.Int32
	legacyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: stream.done\ndata: {}\n\n")
	}))
	defer legacyServer.Close()
	legacyMux := AuthHandler(auth)
	if err := RegisterStream(legacyMux, auth, Access{Store: users}, legacyServer.URL); err != nil {
		t.Fatal(err)
	}
	goMux := mux
	mux = legacyMux
	userToken, _ := auth.Sign(users.user)
	before := calls.Load()
	if w := request("POST", "/api/agent-runs/stream", "rollback-route-key", `{"agentId":"a1","message":"rollback"}`, nil, userToken); w.Code != 200 || legacyCalls.Load() != 1 || calls.Load() != before {
		t.Fatal("legacy routing rollback", w.Code, w.Body.String())
	}
	mux = goMux
	if w := request("POST", path, "public-same-key", `{"message":"hello"}`, a, ""); w.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != before {
		t.Fatal("restored Go route lost replay")
	}
	// Version-pointer rollback changes future executions, not persisted run snapshots.
	exec(`INSERT INTO "AgentVersion" VALUES('v7-rollback','tool-agent',$1)`, strings.Replace(snapshot, "frozen prompt", "rollback prompt", 1))
	exec(`UPDATE "Agent" SET "currentVersionId"='v7-rollback' WHERE "id"='tool-agent'`)
	rolled, _, err := store.BeginRun(ctx, "u1", "rollback-version-key", RunSubmission{AgentID: "tool-agent", Message: "rollback"})
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Snapshot.Instructions != "rollback prompt" {
		t.Fatal("version rollback")
	}
	store.FinishRun(ctx, rolled.ID, "CANCELLED", RunResult{})
	preserved, err := store.GetRun(ctx, "u1", id)
	if err != nil || preserved.Snapshot.Instructions != "frozen prompt" || preserved.Status != "COMPLETED" {
		t.Fatal("rollback changed historical run", err)
	}
	t.Run("sustained public load", func(t *testing.T) { testPublicLoad(t, store) })
}
