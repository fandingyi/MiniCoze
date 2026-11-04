package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type activeRuns struct {
	mu     sync.Mutex
	cancel map[string]context.CancelFunc
}

func (a *activeRuns) add(id string, c context.CancelFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancel[id] = c
}
func (a *activeRuns) remove(id string) { a.mu.Lock(); defer a.mu.Unlock(); delete(a.cancel, id) }
func (a *activeRuns) stop(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c := a.cancel[id]; c != nil {
		c()
	}
}

func RegisterPersistentRuns(mux *http.ServeMux, auth *Auth, store RunStore, origin, serviceToken string) error {
	return RegisterPersistentRunsWithOptions(mux, auth, store, origin, serviceToken, DefaultRunOptions())
}
func RegisterPersistentRunsWithOptions(mux *http.ServeMux, auth *Auth, store RunStore, origin, serviceToken string, options RunOptions) error {
	if err := options.validate(); err != nil {
		return err
	}
	target, err := url.Parse(origin)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil || (target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.Fragment != "" || len(serviceToken) < 32 {
		return errors.New("persistent runs require valid Python URL and service token")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 10 * time.Second, MaxConnsPerHost: 32, MaxIdleConnsPerHost: 16, IdleConnTimeout: 60 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	active := &activeRuns{cancel: map[string]context.CancelFunc{}}
	slots := make(chan struct{}, options.MaxConcurrent)
	channels := channelAdmission{active: map[string]int{}, limit: options.MaxPerChannel}
	mux.Handle("GET /api/agent-runs/{runId}", auth.RunMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		user := r.Context().Value(userKey{}).(User)
		run, err := store.GetRun(ctx, user.ID, r.PathValue("runId"))
		if err != nil {
			runHTTPError(w, err)
			return
		}
		respond(w, 200, 0, "success", run)
	})))
	mux.Handle("POST /api/agent-runs/{runId}/cancel", auth.RunMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		user := r.Context().Value(userKey{}).(User)
		run, err := store.GetRun(ctx, user.ID, r.PathValue("runId"))
		if err != nil {
			runHTTPError(w, err)
			return
		}
		run, err = store.FinishRun(ctx, run.ID, "CANCELLED", RunResult{ErrorCode: "RUN_CANCELLED"})
		if err != nil {
			runHTTPError(w, err)
			return
		}
		if run.Status == "CANCELLED" {
			active.stop(run.ID)
		}
		respond(w, 200, 0, "success", run)
	})))
	mux.Handle("POST /api/agent-runs/stream", auth.RunMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if options.Lifecycle.Err() != nil {
			respond(w, 503, 50300, "服务正在停止", nil)
			return
		}
		requestStarted := time.Now()
		var input RunSubmission
		key := r.Header.Get("Idempotency-Key")
		if len(r.Header.Values("Idempotency-Key")) != 1 || !regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`).MatchString(key) || decode(w, r, &input) != nil || input.AgentID == "" || len(input.AgentID) > 128 || len(input.ConversationID) > 128 || input.Message == "" || len(input.Message) > 32000 {
			respond(w, 400, 40000, "请提供有效 Idempotency-Key、Agent 和消息", nil)
			return
		}
		if access := publicAccess(r.Context()); access != nil {
			release, ok := channels.acquire(access.ChannelID)
			if !ok {
				w.Header().Set("Retry-After", "1")
				respond(w, 429, 42900, "渠道运行并发已满", nil)
				return
			}
			defer release()
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			options.Logger.Warn("run.admission_rejected")
			w.Header().Set("Retry-After", "1")
			respond(w, 429, 42900, "运行并发已满", nil)
			return
		}
		user := r.Context().Value(userKey{}).(User)
		dbCtx, dbCancel := context.WithTimeout(r.Context(), 5*time.Second)
		run, replayed, err := store.BeginRun(dbCtx, user.ID, key, input)
		dbCancel()
		if err != nil {
			runHTTPError(w, err)
			return
		}
		options.Logger.Info("run.accepted", "run_id", run.ID, "replayed", replayed)
		w.Header().Set("X-Run-Id", run.ID)
		w.Header().Set("Location", "/api/agent-runs/"+run.ID)
		if access := publicAccess(r.Context()); access != nil {
			if access.APIHash == "" {
				w.Header().Set("Location", "/api/public/agents/"+url.PathEscape(access.Slug)+"/runs/"+run.ID)
			} else {
				location := "/api/public/agent-runs/" + run.ID
				// Preserve the caller's namespace in the query/cancel URL.
				if visitor := access.VisitorID; visitor != "" {
					location += "?visitorId=" + url.QueryEscape(visitor)
				}
				w.Header().Set("Location", location)
			}
		}
		if replayed {
			w.Header().Set("Idempotency-Replayed", "true")
			if !run.terminal() {
				respond(w, 409, 40900, "运行仍在执行，请查询已有 Run", run)
				return
			}
			_, emit := runEmitter(w, run.ID)
			defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
			_ = emit("run.created", map[string]any{"conversationId": run.ConversationID})
			if run.Status == "COMPLETED" {
				_ = emit("run.in_progress", map[string]any{})
				if run.OutputText != nil {
					_ = emit("message.delta", map[string]any{"messageId": run.assistantID(), "content": *run.OutputText})
				}
			}
			emitRunResult(emit, run)
			return
		}
		// All paths after BeginRun must attempt durable terminal cleanup, including
		// disconnects before Python has accepted the request.
		confirmed := false
		terminalSent := false
		defer func() {
			if !confirmed {
				status, code := "FAILED", "RUN_INTERRUPTED"
				if r.Context().Err() != nil {
					status, code = "CANCELLED", "RUN_CANCELLED"
				}
				if saved, err := finishDetached(store, run.ID, status, RunResult{ErrorCode: code}); err != nil {
					options.Logger.Error("run.cleanup_failed", "run_id", run.ID)
				} else {
					options.Logger.Info("run.cleaned", "run_id", run.ID, "status", saved.Status)
				}
			}
		}()
		dbCtx, dbCancel = context.WithTimeout(r.Context(), 5*time.Second)
		started, err := store.StartRun(dbCtx, run.ID)
		dbCancel()
		if err != nil {
			runHTTPError(w, err)
			return
		}
		run = started
		_, emit := runEmitter(w, run.ID)
		defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
		if err := emit("run.created", map[string]any{"conversationId": run.ConversationID}); err != nil {
			_, _ = finishDetached(store, run.ID, "CANCELLED", RunResult{ErrorCode: "RUN_CANCELLED"})
			return
		}
		if run.terminal() {
			confirmed = true
			emitRunResult(emit, run)
			return
		}
		if run.Status != "RUNNING" {
			_ = emit("run.failed", map[string]any{"error": "运行已过期", "code": "RUN_EXPIRED"})
			_ = emit("stream.done", map[string]any{})
			return
		}
		deadline := run.CreatedAt.Add(options.Timeout)
		if localLimit := requestStarted.Add(options.Timeout); localLimit.Before(deadline) {
			deadline = localLimit
		}
		if cutoff := run.DeadlineAt.Add(-10 * time.Second); cutoff.Before(deadline) {
			deadline = cutoff
		}
		timed, stopTimer := context.WithDeadline(r.Context(), deadline)
		defer stopTimer()
		ctx, cancelCause := context.WithCancelCause(timed)
		cancel := func() { cancelCause(context.Canceled) }
		defer cancel()
		stopLifecycle := context.AfterFunc(options.Lifecycle, func() { cancelCause(errServiceShutdown) })
		defer stopLifecycle()
		active.add(run.ID, cancel)
		defer active.remove(run.ID)
		// Persisted cancellation from another API instance also closes this owner's
		// Python connection. No in-memory state is the authority for the terminal.
		go watchRun(ctx, store, user.ID, run.ID, cancelCause, options.PollInterval)
		settle := func(status string, result RunResult) {
			if terminalSent {
				return
			}
			if ctx.Err() != nil && status == "COMPLETED" {
				var code string
				status, code = executionFailure(r.Context(), ctx, false)
				result = RunResult{ErrorCode: code}
			}
			if result.ErrorCode != "" {
				result.ErrorCode = safeRunCode(result.ErrorCode)
			}
			terminalSent = true
			saved, saveErr := finishDetached(store, run.ID, status, result)
			if saveErr != nil {
				checkCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				saved, saveErr = store.GetRun(checkCtx, user.ID, run.ID)
				stop()
				if saveErr != nil || !saved.terminal() {
					_ = emit("run.failed", map[string]any{"error": "结果持久化未确认，请查询 Run 状态", "code": "RUN_PERSISTENCE_ERROR"})
					_ = emit("stream.done", map[string]any{})
					return
				}
			}
			confirmed = true
			code := ""
			if saved.ErrorCode != nil {
				code = safeRunCode(*saved.ErrorCode)
			}
			options.Logger.Info("run.terminal", "run_id", run.ID, "status", saved.Status, "code", code, "duration_ms", time.Since(requestStarted).Milliseconds())
			if r.Context().Err() == nil {
				emitRunResult(emit, saved)
			}
		}
		remaining := time.Until(deadline) - min(5*time.Second, options.Timeout/10)
		if remaining < 100*time.Millisecond {
			settle("FAILED", RunResult{ErrorCode: "RUN_TIMEOUT"})
			return
		}
		if remaining > 110*time.Second {
			remaining = 110 * time.Second
		}
		command := map[string]any{"schema_version": 1, "run_id": run.ID, "mode": run.Mode, "actor": map[string]string{"user_id": run.UserID, "workspace_id": run.WorkspaceID, "agent_id": run.AgentID, "conversation_id": run.ConversationID}, "snapshot": run.Snapshot.PreviewSnapshot, "history": run.History, "input": map[string]string{"text": run.InputText}, "limits": map[string]int{"deadline_ms": int(remaining.Milliseconds()), "max_output_tokens": run.Snapshot.MaxOutputTokens}}
		body, err := json.Marshal(command)
		if err != nil || len(body) > 256*1024 {
			settle("FAILED", RunResult{ErrorCode: "RUN_INPUT_TOO_LARGE"})
			return
		}
		request, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(origin, "/")+"/internal/v1/runs:stream", bytes.NewReader(body))
		if err != nil {
			settle("FAILED", RunResult{ErrorCode: "RUNTIME_CONFIG_ERROR"})
			return
		}
		request.Header.Set("Authorization", "Bearer "+serviceToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			status, code := "FAILED", "RUNTIME_UNAVAILABLE"
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				code = "RUNTIME_HEADER_TIMEOUT"
			}
			if ctx.Err() != nil {
				status, code = executionFailure(r.Context(), ctx, false)
			}
			settle(status, RunResult{ErrorCode: code})
			return
		}
		defer response.Body.Close()
		if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
			settle("FAILED", RunResult{ErrorCode: "RUNTIME_REJECTED"})
			return
		}
		state := eventState{runID: run.ID}
		deliveryBroken := false
		deliver := func(kind string, payload map[string]any) error {
			err := emit(kind, payload)
			if err != nil {
				deliveryBroken = true
			}
			return err
		}
		err = consumeRuntime(response.Body, &state, func(event runtimeEnvelope) error {
			switch event.Type {
			case "run.created":
				return nil
			case "model.round.completed":
				var round ModelRound
				raw, _ := json.Marshal(event.Payload)
				if json.Unmarshal(raw, &round) != nil {
					return errors.New("invalid round")
				}
				recorder, ok := store.(RoundStore)
				if !ok {
					settle("FAILED", RunResult{ErrorCode: "ROUND_PERSISTENCE_ERROR"})
					cancel()
					return errors.New("round storage unavailable")
				}
				saveCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				err := recorder.RecordModelRound(saveCtx, run.ID, round)
				stop()
				if err != nil {
					settle("FAILED", RunResult{ErrorCode: "ROUND_PERSISTENCE_ERROR"})
					cancel()
					return err
				}
				options.Logger.Info("model.round", "run_id", run.ID, "round", round.Round, "duration_ms", round.DurationMS, "usage_known", round.Usage != nil)
				return nil
			case "message.completed":
				return nil // Final message becomes visible only after commit.
			case "message.delta":
				return deliver(event.Type, map[string]any{"messageId": run.assistantID(), "content": event.Payload["content"]})
			case "run.in_progress":
				return deliver(event.Type, event.Payload)
			case "tool.call.created", "tool.call.completed":
				allowed := false
				for _, capability := range run.Snapshot.Capabilities {
					if capability.Name == event.Payload["name"] {
						allowed = true
						break
					}
				}
				if !allowed {
					return errors.New("runtime emitted an unauthorized tool")
				}
				return deliver(event.Type, event.Payload)
			case "run.completed":
				var usage json.RawMessage
				if value, ok := event.Payload["usage"]; ok {
					usage, _ = json.Marshal(value)
				}
				settle("COMPLETED", RunResult{Text: state.text.String(), Usage: usage})
				cancel()
				return nil
			case "run.failed":
				code, _ := event.Payload["code"].(string)
				settle("FAILED", RunResult{ErrorCode: code})
				cancel()
				return nil
			default:
				return nil
			}
		})
		if !terminalSent {
			status, code := executionFailure(r.Context(), ctx, deliveryBroken)
			settle(status, RunResult{ErrorCode: code})
		}
	})))
	return nil
}

type eventEmitter func(string, map[string]any) error

func runEmitter(w http.ResponseWriter, id string) (*streamWriter, eventEmitter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Run-Persistence", "postgres")
	writer := &streamWriter{ResponseWriter: w}
	sequence := 0
	// Reconstructed result streams are not a replay of the original delta log.
	// Use a separate event namespace so an event ID never identifies two chunks.
	eventPrefix := id
	if w.Header().Get("Idempotency-Replayed") == "true" {
		eventPrefix += ":replay-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return writer, func(kind string, payload map[string]any) error {
		sequence++
		return writeGatewayEvent(writer, runtimeEnvelope{1, fmt.Sprintf("%s:%d", eventPrefix, sequence), id, sequence, time.Now().UTC().Format(time.RFC3339Nano), kind, payload})
	}
}
func emitRunResult(emit eventEmitter, run AgentRun) {
	if run.Status == "COMPLETED" {
		text := ""
		if run.OutputText != nil {
			text = *run.OutputText
		}
		_ = emit("message.completed", map[string]any{"messageId": run.assistantID(), "content": text})
		payload := map[string]any{}
		if len(run.Usage) > 0 {
			var value any
			if json.Unmarshal(run.Usage, &value) == nil && value != nil {
				payload["usage"] = value
			}
		}
		_ = emit("run.completed", payload)
	} else {
		code := "RUN_FAILED"
		if run.ErrorCode != nil {
			code = *run.ErrorCode
		}
		_ = emit("run.failed", map[string]any{"error": "运行未完成，请查看 Run 状态", "code": code, "status": run.Status})
	}
	_ = emit("stream.done", map[string]any{})
}
func runHTTPError(w http.ResponseWriter, err error) {
	var rateLimit *PublicRateLimitError
	switch {
	case errors.As(err, &rateLimit):
		seconds := int(rateLimit.RetryAfter.Seconds())
		if rateLimit.RetryAfter%time.Second != 0 {
			seconds++
		}
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		respond(w, 429, 42901, "Publish channel request quota exceeded", nil)
	case errors.Is(err, ErrNotFound):
		respond(w, 404, 40400, "资源不存在或无权访问", nil)
	case errors.Is(err, ErrForbidden):
		respond(w, 403, 40300, "无权访问该会话", nil)
	case errors.Is(err, ErrKeyConflict):
		respond(w, 409, 40901, "幂等键已用于不同请求", nil)
	case errors.Is(err, ErrRunBusy):
		respond(w, 409, 40902, "该会话已有运行中的请求", nil)
	case errors.Is(err, ErrUnpublished):
		respond(w, 400, 40000, "Agent 尚未发布", nil)
	case errors.Is(err, ErrCapabilities):
		respond(w, 409, 40903, "当前运行链路不支持这些配置或能力", nil)
	default:
		respond(w, 503, 50000, "运行存储暂不可用", nil)
	}
}
func watchRun(ctx context.Context, store RunStore, user, id string, cancel context.CancelCauseFunc, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check, stop := context.WithTimeout(ctx, time.Second)
			run, err := store.GetRun(check, user, id)
			stop()
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
				cancel(errAccessRevoked)
				return
			}
			if err != nil {
				failures++
				if failures >= 3 {
					cancel(errWatchUnavailable)
					return
				}
				continue
			}
			failures = 0
			if run.terminal() {
				cancel(context.Canceled)
				return
			}
		}
	}
}
