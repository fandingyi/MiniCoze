package platform

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Only audited, read-only implementations are exposed. Resource IDs and binding
// policy stay in Go's saved snapshot; the model only sees function definitions.
type Capability struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	ResourceID string `json:"resource_id"`
	ToolID     string `json:"tool_id,omitempty"`
	Code       string `json:"code,omitempty"`
}

func functionDefinition(name, description, argument string) any {
	properties := map[string]any{}
	required := []string{}
	if argument != "" {
		properties[argument] = map[string]any{"type": "string", "minLength": 1, "maxLength": 1000}
		required = append(required, argument)
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description, "parameters": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}}
}

func loadDraftCapabilities(ctx context.Context, tx *sql.Tx, s *SavedSnapshot, selectedKB *string) error {
	var workflow bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "AgentWorkflow" WHERE "agentId"=$1)`, s.AgentID).Scan(&workflow); err != nil {
		return err
	}
	if workflow {
		return ErrCapabilities
	}
	rows, err := tx.QueryContext(ctx, `SELECT p."id",p."code",p."type",p."isBuiltin",p."workspaceId",b."config",t."id",t."code",t."meta" FROM "AgentPluginBinding" b JOIN "PluginDefinition" p ON p."id"=b."pluginId" JOIN "PluginTool" t ON t."pluginId"=p."id" WHERE b."agentId"=$1 AND b."status"='ACTIVE' AND b."autoInvoke"=true AND p."status"='ACTIVE' AND p."invocationEnabled"=true AND t."status"='ACTIVE' ORDER BY p."id",t."id"`, s.AgentID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, code, kind, workspace, toolID, toolCode string
		var builtin bool
		var config, meta []byte
		if err = rows.Scan(&id, &code, &kind, &builtin, &workspace, &config, &toolID, &toolCode, &meta); err != nil {
			rows.Close()
			return err
		}
		var policy struct {
			Disabled []string `json:"disabledTools"`
		}
		if len(config) > 0 && string(config) != "null" {
			if json.Unmarshal(config, &policy) != nil {
				rows.Close()
				return ErrCapabilities
			}
		}
		disabled := false
		for _, v := range policy.Disabled {
			if v == toolCode {
				disabled = true
			}
		}
		if disabled {
			continue
		}
		var metadata struct {
			Handler string `json:"handler"`
		}
		if json.Unmarshal(meta, &metadata) != nil {
			rows.Close()
			return ErrCapabilities
		}
		if workspace != s.WorkspaceID || !builtin || kind != "BUILTIN" || code != "system_tools" || (toolCode != "time_now" && toolCode != "echo_text") || metadata.Handler != toolCode {
			rows.Close()
			return ErrCapabilities
		}
		name := code + "__" + toolCode
		argument, description := "", "Return the current UTC time"
		if toolCode == "echo_text" {
			argument, description = "text", "Echo the supplied text"
		}
		s.Capabilities = append(s.Capabilities, Capability{name, "builtin", id, toolID, toolCode})
		s.Tools = append(s.Tools, functionDefinition(name, description, argument))
		if len(s.Capabilities) > 8 {
			rows.Close()
			return ErrCapabilities
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT k."id",k."name" FROM "KnowledgeBaseAgentBinding" b JOIN "KnowledgeBase" k ON k."id"=b."knowledgeBaseId" WHERE b."agentId"=$1 AND b."enabled"=true AND k."status"='ACTIVE' AND k."workspaceId"=$2 ORDER BY k."id"`, s.AgentID, s.WorkspaceID)
	if err != nil {
		return err
	}
	selected := selectedKB == nil || *selectedKB == ""
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if selectedKB != nil && id == *selectedKB {
			selected = true
		}
		sum := sha256.Sum256([]byte(id))
		function := "knowledge__" + hex.EncodeToString(sum[:8])
		s.Capabilities = append(s.Capabilities, Capability{Name: function, Kind: "knowledge", ResourceID: id})
		if len([]rune(name)) > 300 {
			name = string([]rune(name)[:300])
		}
		s.Tools = append(s.Tools, functionDefinition(function, "Search exact keywords in knowledge base: "+name, "query"))
		if len(s.Capabilities) > 8 {
			rows.Close()
			return ErrCapabilities
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !selected {
		return ErrForbidden
	}
	if len(s.Capabilities) > 8 {
		return ErrCapabilities
	}
	return nil
}

type ToolRequest struct {
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type ToolResult struct {
	CallID string          `json:"call_id"`
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Output json.RawMessage `json:"output"`
	Code   string          `json:"code,omitempty"`
}
type ToolStore interface {
	InvokeTool(context.Context, string, ToolRequest) (ToolResult, error)
	ListToolCalls(context.Context, string, string) ([]ToolResult, error)
}

func RegisterToolGateway(mux *http.ServeMux, auth *Auth, store ToolStore, token string) error {
	if len(token) < 32 {
		return errors.New("tool gateway requires internal service token")
	}
	mux.HandleFunc("POST /internal/v1/runs/{runId}/tools", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			respond(w, 401, 40100, "unauthorized", nil)
			return
		}
		var input ToolRequest
		if decode(w, r, &input) != nil || len(input.CallID) < 1 || len(input.CallID) > 128 || len(input.Name) > 128 || len(input.Arguments) > 8192 || !json.Valid(input.Arguments) {
			respond(w, 400, 40000, "invalid tool request", nil)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		result, err := store.InvokeTool(ctx, r.PathValue("runId"), input)
		if err != nil {
			runHTTPError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
	})
	mux.Handle("GET /api/agent-runs/{runId}/tool-calls", auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		user := r.Context().Value(userKey{}).(User)
		calls, err := store.ListToolCalls(ctx, user.ID, r.PathValue("runId"))
		if err != nil {
			runHTTPError(w, err)
			return
		}
		respond(w, 200, 0, "success", calls)
	})))
	return nil
}

func (p Postgres) InvokeTool(ctx context.Context, id string, input ToolRequest) (ToolResult, error) {
	result := ToolResult{CallID: input.CallID, Name: input.Name, Status: "FAILED", Output: json.RawMessage(`null`)}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM "AgentRun" r WHERE r."id"=$1 FOR UPDATE OF r`, id))
	if err != nil {
		return result, err
	}
	if run.Snapshot.Public != nil {
		if err := checkPublicAccess(ctx, tx, run.Snapshot.Public); err != nil {
			return result, err
		}
	}
	if run.Status != "RUNNING" || time.Now().After(run.DeadlineAt) {
		return result, ErrForbidden
	}
	var member bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "WorkspaceMember" m JOIN "User" u ON u."id"=m."userId" WHERE m."workspaceId"=$1 AND m."userId"=$2 AND u."status"='active')`, run.WorkspaceID, run.UserID).Scan(&member); err != nil {
		return result, err
	}
	if !member {
		return result, ErrForbidden
	}
	encoded, _ := json.Marshal(input)
	hash := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(hash[:])
	var priorHash string
	var output []byte
	err = tx.QueryRowContext(ctx, `SELECT "requestHash","status","output","errorCode" FROM "AgentToolCall" WHERE "runId"=$1 AND "callId"=$2`, id, input.CallID).Scan(&priorHash, &result.Status, &output, &result.Code)
	if err == nil {
		if priorHash != fingerprint {
			return result, ErrKeyConflict
		}
		result.Output = output
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM "AgentToolCall" WHERE "runId"=$1`, id).Scan(&count); err != nil {
		return result, err
	}
	if count >= 8 {
		return result, ErrCapabilities
	}
	var cap *Capability
	for i := range run.Snapshot.Capabilities {
		if run.Snapshot.Capabilities[i].Name == input.Name {
			cap = &run.Snapshot.Capabilities[i]
			break
		}
	}
	result.Code = "TOOL_NOT_ALLOWED"
	if cap != nil {
		allowed, err := capabilityStillAllowed(ctx, tx, run, *cap)
		if err != nil {
			return result, err
		}
		if allowed {
			value, code, err := executeReadOnly(ctx, tx, run, *cap, input.Arguments)
			if err != nil {
				return result, err
			}
			result.Code = code
			if code == "" {
				result.Status = "SUCCESS"
				result.Output, _ = json.Marshal(value)
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO "AgentToolCall"("runId","callId","name","requestHash","arguments","status","output","errorCode","createdAt") VALUES($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp())`, id, input.CallID, input.Name, fingerprint, string(input.Arguments), result.Status, string(result.Output), result.Code)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func capabilityStillAllowed(ctx context.Context, tx *sql.Tx, run AgentRun, c Capability) (bool, error) {
	var allowed bool
	if c.Kind == "knowledge" {
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "KnowledgeBaseAgentBinding" b JOIN "KnowledgeBase" k ON k."id"=b."knowledgeBaseId" WHERE b."agentId"=$1 AND k."id"=$2 AND b."enabled"=true AND k."status"='ACTIVE' AND k."workspaceId"=$3)`, run.AgentID, c.ResourceID, run.WorkspaceID).Scan(&allowed)
		return allowed, err
	}
	if c.Kind != "builtin" {
		return false, nil
	}
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "AgentPluginBinding" b JOIN "PluginDefinition" p ON p."id"=b."pluginId" JOIN "PluginTool" t ON t."pluginId"=p."id" WHERE b."agentId"=$1 AND p."id"=$2 AND t."id"=$3 AND p."workspaceId"=$4 AND b."status"='ACTIVE' AND b."autoInvoke"=true AND p."status"='ACTIVE' AND p."invocationEnabled"=true AND p."isBuiltin"=true AND p."type"='BUILTIN' AND p."code"='system_tools' AND t."status"='ACTIVE' AND t."code"=$5 AND t."meta"->>'handler'=$5 AND NOT COALESCE(b."config"->'disabledTools' ? $5,false))`, run.AgentID, c.ResourceID, c.ToolID, run.WorkspaceID, c.Code).Scan(&allowed)
	return allowed, err
}

func executeReadOnly(ctx context.Context, tx *sql.Tx, run AgentRun, c Capability, args json.RawMessage) (any, string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(args, &fields) != nil || fields == nil {
		return nil, "INVALID_TOOL_ARGUMENTS", nil
	}
	if c.Kind == "builtin" && c.Code == "time_now" {
		if len(fields) != 0 {
			return nil, "INVALID_TOOL_ARGUMENTS", nil
		}
		return map[string]string{"now": time.Now().UTC().Format(time.RFC3339)}, "", nil
	}
	key := "text"
	if c.Kind == "knowledge" {
		key = "query"
	}
	var value string
	if len(fields) != 1 || json.Unmarshal(fields[key], &value) != nil || strings.TrimSpace(value) == "" || len([]rune(value)) > 1000 {
		return nil, "INVALID_TOOL_ARGUMENTS", nil
	}
	if c.Kind == "builtin" && c.Code == "echo_text" {
		return map[string]string{"text": value}, "", nil
	}
	if c.Kind != "knowledge" {
		return nil, "TOOL_NOT_ALLOWED", nil
	}
	// Literal substring matching, not SQL LIKE: percent/underscore are not wildcards.
	rows, err := tx.QueryContext(ctx, `SELECT c."id",c."documentId",LEFT(d."name",200),LEFT(c."content",2000),c."index" FROM "KnowledgeChunk" c JOIN "KnowledgeDocument" d ON d."id"=c."documentId" WHERE c."knowledgeBaseId"=$1 AND c."workspaceId"=$2 AND d."knowledgeBaseId"=$1 AND d."workspaceId"=$2 AND c."enabled"=true AND d."status"='READY' AND strpos(lower(c."content"),lower($3))>0 ORDER BY c."documentId",c."index",c."id" LIMIT 5`, c.ResourceID, run.WorkspaceID, strings.TrimSpace(value))
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	citations := []map[string]any{}
	for rows.Next() {
		var id, doc, name, content string
		var index int
		if err = rows.Scan(&id, &doc, &name, &content, &index); err != nil {
			return nil, "", err
		}
		citations = append(citations, map[string]any{"chunkId": id, "documentId": doc, "documentName": name, "knowledgeBaseId": c.ResourceID, "content": content, "index": index})
	}
	return map[string]any{"query": value, "retrievalMode": "literal_keyword", "citations": citations}, "", rows.Err()
}

func (p Postgres) ListToolCalls(ctx context.Context, user, id string) ([]ToolResult, error) {
	if _, err := p.GetRun(ctx, user, id); err != nil {
		return nil, err
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT "callId","name","status","output","errorCode" FROM "AgentToolCall" WHERE "runId"=$1 ORDER BY "createdAt","callId"`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []ToolResult{}
	for rows.Next() {
		var r ToolResult
		var output []byte
		if err = rows.Scan(&r.CallID, &r.Name, &r.Status, &output, &r.Code); err != nil {
			return nil, err
		}
		r.Output = output
		results = append(results, r)
	}
	return results, rows.Err()
}
