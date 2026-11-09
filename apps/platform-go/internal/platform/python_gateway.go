package platform

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type PreviewSnapshot struct {
	AgentID       string `json:"-"`
	WorkspaceID   string `json:"-"`
	SchemaVersion int    `json:"snapshot_schema_version"`
	Model         struct {
		Name        string  `json:"name"`
		Temperature float64 `json:"temperature"`
	} `json:"model"`
	Instructions    string `json:"instructions"`
	Tools           []any  `json:"tools"`
	Knowledge       []any  `json:"knowledge"`
	HasCapabilities bool   `json:"-"`
}
type SnapshotReader interface {
	PreviewSnapshot(context.Context, string, string) (PreviewSnapshot, error)
}

func (p Postgres) PreviewSnapshot(ctx context.Context, user, agent string) (PreviewSnapshot, error) {
	var snapshot PreviewSnapshot
	err := p.DB.QueryRowContext(ctx, `SELECT a."id",a."workspaceId",a."model",a."temperature",a."systemPrompt", EXISTS(SELECT 1 FROM "AgentPluginBinding" b WHERE b."agentId"=a."id") OR EXISTS(SELECT 1 FROM "KnowledgeBaseAgentBinding" k WHERE k."agentId"=a."id") OR EXISTS(SELECT 1 FROM "AgentWorkflow" f WHERE f."agentId"=a."id") FROM "Agent" a JOIN "WorkspaceMember" m ON m."workspaceId"=a."workspaceId" WHERE m."userId"=$1 AND a."id"=$2`, user, agent).Scan(&snapshot.AgentID, &snapshot.WorkspaceID, &snapshot.Model.Name, &snapshot.Model.Temperature, &snapshot.Instructions, &snapshot.HasCapabilities)
	snapshot.SchemaVersion = 1
	snapshot.Tools = []any{}
	snapshot.Knowledge = []any{}
	return snapshot, mapNoRows(err)
}

type runtimeEnvelope struct {
	SchemaVersion int            `json:"schema_version"`
	EventID       string         `json:"event_id"`
	RunID         string         `json:"run_id"`
	Sequence      int            `json:"sequence"`
	Timestamp     string         `json:"timestamp"`
	Type          string         `json:"type"`
	Payload       map[string]any `json:"payload"`
}

// Validate the internal stream before exposing events from another service.
type eventState struct {
	runID           string
	sequence        int
	phase           string
	messageID       string
	text            strings.Builder
	messageComplete bool
	tools           map[string]string
	toolsFinished   map[string]bool
	modelRounds     int
}

func (s *eventState) validate(e runtimeEnvelope) error {
	if e.SchemaVersion != 1 || e.RunID != s.runID || e.Sequence != s.sequence+1 || e.EventID != fmt.Sprintf("%s:%d", s.runID, e.Sequence) || e.Payload == nil {
		return errors.New("invalid envelope")
	}
	if _, err := time.Parse(time.RFC3339Nano, e.Timestamp); err != nil {
		return err
	}
	stringField := func(key string) (string, bool) { v, ok := e.Payload[key].(string); return v, ok }
	allowed := map[string][]string{
		"run.created": {"conversationId"}, "run.in_progress": {},
		"message.delta": {"messageId", "content"}, "message.completed": {"messageId", "content"},
		"run.completed": {"usage", "finishReason"}, "run.failed": {"error", "code"}, "stream.done": {},
		"tool.call.created": {"toolCallId", "name", "args"}, "tool.call.completed": {"toolCallId", "name", "result", "error"},
		"model.round.completed": {"round", "durationMs", "finishReason", "usage"},
	}
	for key := range e.Payload {
		if !slices.Contains(allowed[e.Type], key) {
			return errors.New("unexpected event field")
		}
	}
	switch e.Type {
	case "model.round.completed":
		if s.phase != "running" || s.messageComplete {
			return errors.New("invalid model round state")
		}
		var round ModelRound
		raw, _ := json.Marshal(e.Payload)
		if json.Unmarshal(raw, &round) != nil || !round.valid() || round.Round != s.modelRounds+1 {
			return errors.New("invalid model round receipt")
		}
		s.modelRounds++
	case "tool.call.created", "tool.call.completed":
		if s.phase != "running" || s.messageComplete {
			return errors.New("invalid tool event state")
		}
		id, ok := stringField("toolCallId")
		name, nameOK := stringField("name")
		if !ok || !nameOK || id == "" || len(id) > 128 || name == "" || len(name) > 64 {
			return errors.New("invalid tool identity")
		}
		if s.tools == nil {
			s.tools = map[string]string{}
			s.toolsFinished = map[string]bool{}
		}
		if e.Type == "tool.call.created" {
			if len(s.tools) >= 8 || s.tools[id] != "" {
				return errors.New("duplicate or excess tool call")
			}
			if _, ok := e.Payload["args"].(map[string]any); !ok {
				return errors.New("invalid tool arguments")
			}
			s.tools[id] = name
		} else {
			if s.tools[id] != name || s.toolsFinished[id] {
				return errors.New("unknown or completed tool call")
			}
			if _, ok := e.Payload["result"]; !ok {
				return errors.New("missing tool result")
			}
			if _, exists := e.Payload["error"]; exists {
				if _, ok := stringField("error"); !ok {
					return errors.New("invalid tool error")
				}
			}
			s.toolsFinished[id] = true
		}
	case "run.created":
		if s.phase != "" {
			return errors.New("duplicate run")
		}
		if _, ok := stringField("conversationId"); !ok {
			return errors.New("missing conversation")
		}
		s.phase = "created"
	case "run.in_progress":
		if s.phase != "created" {
			return errors.New("invalid start")
		}
		s.phase = "running"
	case "message.delta", "message.completed":
		if s.phase != "running" || s.messageComplete {
			return errors.New("invalid message state")
		}
		id, ok := stringField("messageId")
		if !ok || id == "" || (s.messageID != "" && s.messageID != id) {
			return errors.New("invalid message ID")
		}
		s.messageID = id
		content, ok := stringField("content")
		if !ok || len(content) > 1<<20 {
			return errors.New("invalid content")
		}
		if e.Type == "message.delta" {
			if s.text.Len()+len(content) > 1<<20 {
				return errors.New("output too large")
			}
			s.text.WriteString(content)
		} else {
			if content != s.text.String() {
				return errors.New("final content mismatch")
			}
			s.messageComplete = true
		}
	case "run.completed":
		if len(s.tools) != len(s.toolsFinished) {
			return errors.New("unfinished tool call")
		}
		if s.phase != "running" || !s.messageComplete {
			return errors.New("premature success")
		}
		if raw, exists := e.Payload["usage"]; exists {
			usage, ok := raw.(map[string]any)
			if !ok || len(usage) != 3 {
				return errors.New("invalid usage")
			}
			for _, key := range []string{"inputTokens", "outputTokens", "totalTokens"} {
				value, ok := usage[key].(float64)
				if !ok || value < 0 || math.Trunc(value) != value {
					return errors.New("invalid usage")
				}
			}
		}
		if reason, exists := e.Payload["finishReason"]; exists && reason != "stop" && reason != "length" {
			return errors.New("invalid finish reason")
		}
		s.phase = "terminal"
	case "run.failed":
		if s.phase != "created" && s.phase != "running" {
			return errors.New("invalid failure")
		}
		if _, ok := stringField("error"); !ok {
			return errors.New("missing error")
		}
		if code, exists := e.Payload["code"]; exists {
			value, ok := code.(string)
			if !ok || value == "" || len(value) > 64 {
				return errors.New("invalid failure code")
			}
		}
		s.phase = "terminal"
	case "stream.done":
		if s.phase != "terminal" {
			return errors.New("missing terminal")
		}
		s.phase = "done"
	default:
		return errors.New("unsupported event")
	}
	s.sequence = e.Sequence
	return nil
}

func RegisterPythonPreview(mux *http.ServeMux, auth *Auth, store SnapshotReader, origin, token string) error {
	target, err := url.Parse(origin)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil || (target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.Fragment != "" || len(token) < 32 {
		return errors.New("configure PYTHON_RUNTIME_URL and RUNTIME_SERVICE_TOKEN (32+ characters)")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 10 * time.Second, MaxConnsPerHost: 16, MaxIdleConnsPerHost: 16, IdleConnTimeout: 60 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	slots := make(chan struct{}, 16)
	mux.Handle("POST /api/agent-runs/preview/stream", auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			AgentID string `json:"agentId"`
			Message string `json:"message"`
		}
		if decode(w, r, &input) != nil || input.AgentID == "" || input.Message == "" || utf8.RuneCountInString(input.Message) > 32000 {
			respond(w, 400, 40000, "预览参数错误", nil)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		user := r.Context().Value(userKey{}).(User)
		dbCtx, dbCancel := context.WithTimeout(ctx, 5*time.Second)
		snapshot, err := store.PreviewSnapshot(dbCtx, user.ID, input.AgentID)
		dbCancel()
		if errors.Is(err, ErrNotFound) {
			respond(w, 404, 40400, "Agent 不存在或无权访问", nil)
			return
		}
		if err != nil {
			respond(w, 503, 50000, "配置读取失败", nil)
			return
		}
		if snapshot.HasCapabilities {
			respond(w, 409, 40000, "该预览入口暂不支持工具、知识库或工作流，请使用原运行入口", nil)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			respond(w, 429, 42900, "预览并发已满", nil)
			return
		}
		runID, err := randomID()
		if err != nil {
			respond(w, 500, 50000, "无法创建预览", nil)
			return
		}
		body, err := json.Marshal(map[string]any{"schema_version": 1, "run_id": runID, "mode": "preview", "actor": map[string]string{"user_id": user.ID, "workspace_id": snapshot.WorkspaceID, "agent_id": snapshot.AgentID}, "snapshot": snapshot, "input": map[string]string{"text": input.Message}, "limits": map[string]int{"deadline_ms": 110000, "max_output_tokens": 2048}})
		if err != nil {
			respond(w, 500, 50000, "配置无效", nil)
			return
		}
		request, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(origin, "/")+"/internal/v1/runs:stream", bytes.NewReader(body))
		if err != nil {
			respond(w, 500, 50000, "网关配置错误", nil)
			return
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			if r.Context().Err() == nil {
				respond(w, 502, 50000, "Python Runtime 不可用", nil)
			}
			return
		}
		defer response.Body.Close()
		if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
			respond(w, 502, 50000, "Python Runtime 拒绝运行，请检查模型和内部服务配置", nil)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("X-Run-Persistence", "none")
		writer := &streamWriter{ResponseWriter: w}
		defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
		state := eventState{runID: runID}
		err = forwardRuntime(response.Body, writer, &state)
		if r.Context().Err() != nil {
			return
		}
		if err != nil && state.phase != "terminal" && state.phase != "done" {
			state.sequence++
			_ = writeGatewayEvent(writer, runtimeEnvelope{1, fmt.Sprintf("%s:%d", runID, state.sequence), runID, state.sequence, time.Now().UTC().Format(time.RFC3339Nano), "run.failed", map[string]any{"error": "执行流中断或协议无效", "code": "RUNTIME_PROTOCOL_ERROR"}})
		}
		if state.phase != "done" {
			state.sequence++
			_ = writeGatewayEvent(writer, runtimeEnvelope{1, fmt.Sprintf("%s:%d", runID, state.sequence), runID, state.sequence, time.Now().UTC().Format(time.RFC3339Nano), "stream.done", map[string]any{}})
		}
	})))
	return nil
}

func writeGatewayEvent(w *streamWriter, e runtimeEnvelope) error {
	flat := make(map[string]any, len(e.Payload)+7)
	for k, v := range e.Payload {
		flat[k] = v
	}
	flat["type"] = e.Type
	flat["runId"] = e.RunID
	flat["schema_version"] = e.SchemaVersion
	flat["event_id"] = e.EventID
	flat["sequence"] = e.Sequence
	flat["timestamp"] = e.Timestamp
	body, err := json.Marshal(flat)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.EventID, e.Type, body); err != nil {
		return err
	}
	return w.FlushError()
}
func forwardRuntime(body io.Reader, w *streamWriter, state *eventState) error {
	return consumeRuntime(body, state, func(event runtimeEnvelope) error { return writeGatewayEvent(w, event) })
}

func consumeRuntime(body io.Reader, state *eventState, onEvent func(runtimeEnvelope) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			if data.Len()+len(line) > 2<<20 {
				return errors.New("frame too large")
			}
			data.WriteString(strings.TrimSpace(line[5:]))
			data.WriteByte('\n')
		}
		if line == "" && data.Len() > 0 {
			var event runtimeEnvelope
			if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
				return err
			}
			data.Reset()
			if err := state.validate(event); err != nil {
				return err
			}
			if err := onEvent(event); err != nil {
				return err
			}
			if state.phase == "done" {
				return nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}
