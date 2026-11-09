package platform

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in, bounded and isolated: only the disposable test database and fake runtime.
func testPublicLoad(t *testing.T, store Postgres) {
	if os.Getenv("RUN_PUBLIC_LOAD") != "1" {
		t.Skip("set RUN_PUBLIC_LOAD=1 for 30s public HTTP load")
	}
	const workers = 8
	const limit = 4
	const duration = 30 * time.Second
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			RunID string `json:"run_id"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		upstreamCalls.Add(1)
		select {
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, normalStream(input.RunID))
	}))
	defer upstream.Close()
	auth, _ := setup(t)
	mux := AuthHandler(auth)
	options := DefaultRunOptions()
	options.MaxConcurrent = limit
	options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := RegisterPersistentRunsWithOptions(mux, auth, store, upstream.URL, "test-runtime-service-token-32-characters", options); err != nil {
		t.Fatal(err)
	}
	if err := RegisterPublicRoutes(mux, store, "test-public-session-secret-32-characters", false); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(mux)
	defer gateway.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	type sample struct {
		status  int
		elapsed time.Duration
		valid   bool
	}
	samples := make(chan sample, workers)
	var wg sync.WaitGroup
	start := time.Now()
	end := start.Add(duration)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := 0; time.Now().Before(end); n++ {
				req, _ := http.NewRequest("POST", gateway.URL+"/api/public/agent-runs/stream", strings.NewReader(`{"message":"public-load-acceptance"}`))
				req.Header.Set("Authorization", "Bearer rotated-api-key-day-seven")
				req.Header.Set("Idempotency-Key", fmt.Sprintf("public-load-%d-%d", worker, n))
				began := time.Now()
				res, err := client.Do(req)
				if err != nil {
					samples <- sample{0, time.Since(began), false}
					continue
				}
				data, readErr := io.ReadAll(res.Body)
				res.Body.Close()
				valid := readErr == nil && (res.StatusCode == 429 || (res.StatusCode == 200 && strings.Count(string(data), "event: run.completed\n") == 1 && strings.Count(string(data), "event: stream.done\n") == 1 && !strings.Contains(string(data), "event: run.failed")))
				samples <- sample{res.StatusCode, time.Since(began), valid}
				if res.StatusCode == 429 {
					time.Sleep(10 * time.Millisecond)
				}
			}
		}(worker)
	}
	go func() { wg.Wait(); close(samples) }()
	completed, rejected, failed := 0, 0, 0
	timings := []time.Duration{}
	for s := range samples {
		if !s.valid {
			failed++
		} else if s.status == 429 {
			rejected++
		} else {
			completed++
			timings = append(timings, s.elapsed)
		}
	}
	wall := time.Since(start)
	var rows, active, assistant int
	if err := store.DB.QueryRow(`SELECT count(*),count(*) FILTER (WHERE "status" IN ('CREATED','RUNNING')) FROM "AgentRun" WHERE "inputText"='public-load-acceptance'`).Scan(&rows, &active); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(`SELECT count(*) FROM "Message" m JOIN "AgentRun" r ON r."id"=m."runId" WHERE r."inputText"='public-load-acceptance' AND m."role"='ASSISTANT'`).Scan(&assistant); err != nil {
		t.Fatal(err)
	}
	if failed != 0 || completed == 0 || rejected == 0 || rows != completed || assistant != completed || active != 0 || upstreamCalls.Load() != int64(completed) {
		t.Fatalf("load invariant: completed=%d rejected=%d failed=%d rows=%d assistant=%d active=%d upstream=%d", completed, rejected, failed, rows, assistant, active, upstreamCalls.Load())
	}
	sort.Slice(timings, func(i, j int) bool { return timings[i] < timings[j] })
	report := map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339), "scope": "localhost HTTP + disposable PostgreSQL + mock Runtime with 100ms delay; no real model", "workers": workers, "admission_limit": limit, "duration_seconds": wall.Seconds(), "completed": completed, "limited_429": rejected, "unexpected_failures": failed, "completed_per_second": float64(completed) / wall.Seconds(), "success_p50_ms": float64(timings[(len(timings)-1)/2].Microseconds()) / 1000, "success_p95_ms": float64(timings[(len(timings)*95+99)/100-1].Microseconds()) / 1000, "remaining_active_runs": active, "assistant_messages": assistant}
	output, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join("..", "..", "..", "..", "artifacts", "day-7", "public-load.json")
	if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, append(output, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	t.Log(string(output))
}
