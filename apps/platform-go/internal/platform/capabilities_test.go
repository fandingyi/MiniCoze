package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func testToolGateway(t *testing.T, p Postgres) {
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := p.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`ALTER TABLE "AgentPluginBinding" ADD COLUMN "pluginId" text,ADD COLUMN "status" text,ADD COLUMN "autoInvoke" boolean,ADD COLUMN "config" jsonb;
 ALTER TABLE "KnowledgeBaseAgentBinding" ADD COLUMN "knowledgeBaseId" text,ADD COLUMN "enabled" boolean;
 CREATE TABLE "PluginDefinition"("id" text PRIMARY KEY,"code" text,"type" text,"isBuiltin" boolean,"workspaceId" text,"status" text,"invocationEnabled" boolean);
 CREATE TABLE "PluginTool"("id" text PRIMARY KEY,"pluginId" text,"code" text,"meta" jsonb,"status" text);
 CREATE TABLE "KnowledgeBase"("id" text PRIMARY KEY,"name" text,"workspaceId" text,"status" text);
 CREATE TABLE "KnowledgeDocument"("id" text PRIMARY KEY,"knowledgeBaseId" text,"workspaceId" text,"name" text,"status" text);
 CREATE TABLE "KnowledgeChunk"("id" text PRIMARY KEY,"documentId" text,"knowledgeBaseId" text,"workspaceId" text,"content" text,"index" int,"enabled" boolean);
 INSERT INTO "Agent" VALUES('tool-agent','w1','test-model',0.7,'tools',10,'ACTIVE',NULL);
 INSERT INTO "PluginDefinition" VALUES('plugin','system_tools','BUILTIN',true,'w1','ACTIVE',true);
 INSERT INTO "PluginTool" VALUES('echo','plugin','echo_text','{"handler":"echo_text"}','ACTIVE'),('time','plugin','time_now','{"handler":"time_now"}','ACTIVE');
 INSERT INTO "AgentPluginBinding" VALUES('tool-agent','plugin','ACTIVE',true,'{}');
 INSERT INTO "KnowledgeBase" VALUES('kb','handbook','w1','ACTIVE');
 INSERT INTO "KnowledgeBaseAgentBinding" VALUES('tool-agent','kb',true);
 INSERT INTO "KnowledgeDocument" VALUES('doc','kb','w1','guide','READY'),('hidden-doc','kb','w1','hidden','PROCESSING');
 INSERT INTO "KnowledgeChunk" VALUES('chunk','doc','kb','w1','MiniCoze supports Go agents',0,true),('disabled','doc','kb','w1','MiniCoze secret',1,false),('hidden','hidden-doc','kb','w1','MiniCoze hidden',0,true),('foreign','doc','kb','w2','MiniCoze foreign',0,true);`)
	ctx := context.Background()
	begin := func(key string) AgentRun {
		t.Helper()
		r, _, err := p.BeginRun(ctx, "u1", key, RunSubmission{AgentID: "tool-agent", Preview: true, Message: "MiniCoze"})
		if err != nil {
			t.Fatal(err)
		}
		r, err = p.StartRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	run := begin("tools-key")
	exec(`UPDATE "User" SET "status"='disabled' WHERE "id"='u1'`)
	if _, err := p.InvokeTool(ctx, run.ID, ToolRequest{"disabled-user", "system_tools__time_now", json.RawMessage(`{}`)}); !errors.Is(err, ErrForbidden) {
		t.Fatal("disabled account executed tool", err)
	}
	exec(`UPDATE "User" SET "status"='active' WHERE "id"='u1'`)
	if len(run.Snapshot.Capabilities) != 3 || len(run.Snapshot.Tools) != 3 {
		t.Fatal("missing capabilities")
	}
	invoke := func(id, name, args string) ToolResult {
		t.Helper()
		r, err := p.InvokeTool(ctx, run.ID, ToolRequest{id, name, json.RawMessage(args)})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := p.InvokeTool(ctx, run.ID, ToolRequest{"echo-call", "system_tools__echo_text", json.RawMessage(`{"text":"hello"}`)})
			if err != nil || r.Status != "SUCCESS" {
				t.Error("concurrent call", r, err)
			}
		}()
	}
	wg.Wait()
	var count int
	if err := p.DB.QueryRow(`SELECT COUNT(*) FROM "AgentToolCall" WHERE "runId"=$1`, run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate tool execution", count, err)
	}
	if _, err := p.InvokeTool(ctx, run.ID, ToolRequest{"echo-call", "system_tools__echo_text", json.RawMessage(`{"text":"different"}`)}); !errors.Is(err, ErrKeyConflict) {
		t.Fatal("call conflict", err)
	}
	knowledge := ""
	for _, c := range run.Snapshot.Capabilities {
		if c.Kind == "knowledge" {
			knowledge = c.Name
		}
	}
	result := invoke("kb-call", knowledge, `{"query":"MiniCoze"}`)
	if result.Status != "SUCCESS" || !strings.Contains(string(result.Output), `"chunkId":"chunk"`) || strings.Contains(string(result.Output), "secret") || strings.Contains(string(result.Output), "hidden") || strings.Contains(string(result.Output), "foreign") {
		t.Fatal("knowledge isolation", string(result.Output))
	}
	if r := invoke("wildcard", knowledge, `{"query":"%"}`); !strings.Contains(string(r.Output), `"citations":[]`) {
		t.Fatal("query wildcard", string(r.Output))
	}
	if r := invoke("unbound", "system_tools__delete", `{}`); r.Code != "TOOL_NOT_ALLOWED" {
		t.Fatal("unbound tool")
	}
	if r := invoke("bad-args", "system_tools__time_now", `{"url":"http://example.com"}`); r.Code != "INVALID_TOOL_ARGUMENTS" {
		t.Fatal("argument validation")
	}
	exec(`UPDATE "AgentPluginBinding" SET "config"='{"disabledTools":["echo_text"]}' WHERE "agentId"='tool-agent'`)
	if r := invoke("disabled", "system_tools__echo_text", `{"text":"hello"}`); r.Code != "TOOL_NOT_ALLOWED" {
		t.Fatal("revoked tool")
	}
	exec(`UPDATE "KnowledgeBaseAgentBinding" SET "enabled"=false WHERE "agentId"='tool-agent'`)
	if r := invoke("revoked-kb", knowledge, `{"query":"MiniCoze"}`); r.Code != "TOOL_NOT_ALLOWED" {
		t.Fatal("revoked knowledge")
	}
	invoke("last", "system_tools__time_now", `{}`)
	if _, err := p.InvokeTool(ctx, run.ID, ToolRequest{"over-budget", "system_tools__time_now", json.RawMessage(`{}`)}); !errors.Is(err, ErrCapabilities) {
		t.Fatal("budget", err)
	}
	calls, err := p.ListToolCalls(ctx, "u1", run.ID)
	if err != nil || len(calls) != 8 {
		t.Fatal("audit", len(calls), err)
	}
	if _, err = p.ListToolCalls(ctx, "outsider", run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("audit leaked", err)
	}
	auth, users := setup(t)
	mux := AuthHandler(auth)
	if err = RegisterToolGateway(mux, auth, p, "test-runtime-service-token-32-characters"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/internal/v1/runs/"+run.ID+"/tools", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("internal auth")
	}
	userToken, _ := auth.Sign(users.user)
	req = httptest.NewRequest("POST", "/internal/v1/runs/"+run.ID+"/tools", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+userToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("user token accepted as service")
	}
	_, err = p.FinishRun(ctx, run.ID, "CANCELLED", RunResult{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.InvokeTool(ctx, run.ID, ToolRequest{"after-cancel", "system_tools__time_now", json.RawMessage(`{}`)}); !errors.Is(err, ErrForbidden) {
		t.Fatal("terminal executed tool", err)
	}
	// A later run never gains a disabled capability from client-provided tool JSON.
	run = begin("tools-next")
	if len(run.Snapshot.Capabilities) != 1 {
		t.Fatal("disabled capability advertised")
	}
	_, err = p.FinishRun(ctx, run.ID, "CANCELLED", RunResult{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(fmt.Sprintf("verified %d persisted calls, budgets, revocation, keyword citations and isolation", len(calls)))
	exec(`UPDATE "AgentPluginBinding" SET "config"='{}' WHERE "agentId"='tool-agent'; UPDATE "KnowledgeBaseAgentBinding" SET "enabled"=true WHERE "agentId"='tool-agent'`)
	t.Run("real Go Python tool chain", func(t *testing.T) { testRealToolChain(t, p) })
}
