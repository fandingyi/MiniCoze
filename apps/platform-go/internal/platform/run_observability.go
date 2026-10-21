package platform

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

var errServiceShutdown = errors.New("service shutdown")
var errAccessRevoked = errors.New("run access revoked")
var errWatchUnavailable = errors.New("run status unavailable")

type RunOptions struct {
	Timeout       time.Duration
	PollInterval  time.Duration
	MaxConcurrent int
	Lifecycle     context.Context
	Logger        *slog.Logger
}

func DefaultRunOptions() RunOptions {
	return RunOptions{120 * time.Second, time.Second, 32, context.Background(), slog.Default()}
}
func (o RunOptions) validate() error {
	if o.Timeout < 100*time.Millisecond || o.Timeout > 120*time.Second || o.PollInterval < 10*time.Millisecond || o.PollInterval > 5*time.Second || o.MaxConcurrent < 1 || o.MaxConcurrent > 256 || o.Lifecycle == nil || o.Logger == nil {
		return errors.New("invalid run lifecycle options")
	}
	return nil
}
func executionFailure(request, execution context.Context, deliveryBroken bool) (string, string) {
	switch {
	case errors.Is(context.Cause(execution), errServiceShutdown):
		return "FAILED", "SERVICE_SHUTDOWN"
	case errors.Is(context.Cause(execution), errAccessRevoked):
		return "CANCELLED", "RUN_ACCESS_REVOKED"
	case errors.Is(context.Cause(execution), errWatchUnavailable):
		return "FAILED", "RUN_STATUS_UNAVAILABLE"
	case request.Err() != nil:
		return "CANCELLED", "CLIENT_DISCONNECTED"
	case deliveryBroken:
		return "CANCELLED", "CLIENT_WRITE_FAILED"
	case errors.Is(execution.Err(), context.DeadlineExceeded):
		return "FAILED", "RUN_TIMEOUT"
	default:
		return "FAILED", "RUNTIME_PROTOCOL_ERROR"
	}
}

// Only known public codes may leave the runtime or enter logs. Never log provider
// responses, prompts, arguments, output text, credentials or raw errors.
func safeRunCode(code string) string {
	switch code {
	case "MODEL_ERROR", "MODEL_TIMEOUT", "TOOL_GATEWAY_ERROR", "TOOL_TIMEOUT", "RUN_TIMEOUT", "RUNTIME_PROTOCOL_ERROR", "RUN_CANCELLED", "CLIENT_DISCONNECTED", "CLIENT_WRITE_FAILED", "RUN_ACCESS_REVOKED", "RUN_STATUS_UNAVAILABLE", "SERVICE_SHUTDOWN", "RUN_EXPIRED", "RUN_INTERRUPTED", "RUNTIME_UNAVAILABLE", "RUNTIME_HEADER_TIMEOUT", "RUNTIME_REJECTED", "RUN_INPUT_TOO_LARGE", "RUNTIME_CONFIG_ERROR", "RUN_PERSISTENCE_ERROR", "ROUND_PERSISTENCE_ERROR":
		return code
	default:
		return "RUNTIME_ERROR"
	}
}

type TokenCounts struct {
	Input  int64 `json:"inputTokens"`
	Output int64 `json:"outputTokens"`
	Total  int64 `json:"totalTokens"`
}
type ModelRound struct {
	Round        int          `json:"round"`
	DurationMS   int64        `json:"durationMs"`
	FinishReason string       `json:"finishReason"`
	Usage        *TokenCounts `json:"usage"`
}

func (r ModelRound) valid() bool {
	return r.Round >= 1 && r.Round <= 5 && r.DurationMS >= 0 && r.DurationMS <= 120000 && (r.FinishReason == "stop" || r.FinishReason == "length" || r.FinishReason == "tool_calls") && (r.Usage == nil || (r.Usage.Input >= 0 && r.Usage.Output >= 0 && r.Usage.Total >= 0 && r.Usage.Input <= 1e12 && r.Usage.Output <= 1e12 && r.Usage.Total <= 1e12))
}

type RunMetrics struct {
	RunID         string       `json:"runId"`
	Status        string       `json:"status"`
	DurationMS    int64        `json:"durationMs"`
	Rounds        []ModelRound `json:"rounds"`
	ReportedUsage *TokenCounts `json:"reportedUsage"`
	UsageComplete bool         `json:"usageComplete"`
	ToolCalls     int          `json:"toolCalls"`
}
type RoundStore interface {
	RecordModelRound(context.Context, string, ModelRound) error
	RunMetrics(context.Context, string, string) (RunMetrics, error)
}

func (p Postgres) RecordModelRound(ctx context.Context, id string, round ModelRound) error {
	if !round.valid() {
		return errors.New("invalid model round")
	}
	var usage any
	if round.Usage != nil {
		b, _ := json.Marshal(round.Usage)
		usage = string(b)
	}
	// Round receipts are immutable; a network retry can only repeat the same data.
	result, err := p.DB.ExecContext(ctx, `INSERT INTO "AgentModelRound"("runId","round","durationMs","finishReason","usage") VALUES($1,$2,$3,$4,$5) ON CONFLICT ("runId","round") DO NOTHING`, id, round.Round, round.DurationMS, round.FinishReason, usage)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 1 {
		return err
	}
	var equal bool
	err = p.DB.QueryRowContext(ctx, `SELECT "durationMs"=$3 AND "finishReason"=$4 AND "usage" IS NOT DISTINCT FROM $5::jsonb FROM "AgentModelRound" WHERE "runId"=$1 AND "round"=$2`, id, round.Round, round.DurationMS, round.FinishReason, usage).Scan(&equal)
	if err != nil {
		return err
	}
	if !equal {
		return ErrKeyConflict
	}
	return nil
}
func (p Postgres) RunMetrics(ctx context.Context, user, id string) (RunMetrics, error) {
	run, err := p.GetRun(ctx, user, id)
	if err != nil {
		return RunMetrics{}, err
	}
	end := time.Now()
	if run.FinishedAt != nil {
		end = *run.FinishedAt
	}
	result := RunMetrics{RunID: id, Status: run.Status, DurationMS: max(int64(0), end.Sub(run.CreatedAt).Milliseconds()), Rounds: []ModelRound{}}
	rows, err := p.DB.QueryContext(ctx, `SELECT "round","durationMs","finishReason","usage" FROM "AgentModelRound" WHERE "runId"=$1 ORDER BY "round"`, id)
	if err != nil {
		return result, err
	}
	allKnown := true
	for rows.Next() {
		var r ModelRound
		var raw []byte
		if err = rows.Scan(&r.Round, &r.DurationMS, &r.FinishReason, &raw); err != nil {
			rows.Close()
			return result, err
		}
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &r.Usage); err != nil {
				rows.Close()
				return result, err
			}
		}
		if r.Usage == nil {
			allKnown = false
		} else {
			if result.ReportedUsage == nil {
				result.ReportedUsage = &TokenCounts{}
			}
			result.ReportedUsage.Input += r.Usage.Input
			result.ReportedUsage.Output += r.Usage.Output
			result.ReportedUsage.Total += r.Usage.Total
		}
		result.Rounds = append(result.Rounds, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	count := len(result.Rounds)
	contiguous := true
	for i, r := range result.Rounds {
		if r.Round != i+1 {
			contiguous = false
		}
	}
	result.UsageComplete = run.Status == "COMPLETED" && count > 0 && allKnown && contiguous && result.Rounds[count-1].FinishReason != "tool_calls"
	err = p.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM "AgentToolCall" WHERE "runId"=$1`, id).Scan(&result.ToolCalls)
	return result, err
}
func RegisterRunMetrics(mux *http.ServeMux, auth *Auth, store RoundStore) {
	mux.Handle("GET /api/agent-runs/{runId}/metrics", auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		user := r.Context().Value(userKey{}).(User)
		metrics, err := store.RunMetrics(ctx, user.ID, r.PathValue("runId"))
		if err != nil {
			runHTTPError(w, err)
			return
		}
		respond(w, 200, 0, "success", metrics)
	})))
}

// Batches skip rows already locked by another execution/reaper instance.
func RunReaper(ctx context.Context, store RunStore, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check, cancel := context.WithTimeout(ctx, 5*time.Second)
			n, err := store.ExpireRuns(check)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				logger.Warn("run.reaper_failed")
			} else if n > 0 {
				logger.Info("run.reaped", "count", n)
			}
		}
	}
}
