package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testRealToolChain(t *testing.T, store Postgres, published ...bool) {
	preview := true
	suffix := ""
	if len(published) > 0 && published[0] {
		preview = false
		suffix = "-published"
	}
	if os.Getenv("RUN_PYTHON_INTEGRATION") != "1" {
		t.Skip("set RUN_PYTHON_INTEGRATION=1")
	}
	auth, users := setup(t)
	token, _ := auth.Sign(users.user)
	mux := AuthHandler(auth)
	const serviceToken = "test-runtime-service-token-32-characters"
	if err := RegisterToolGateway(mux, auth, store, serviceToken); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(mux)
	defer gateway.Close()
	var modelCalls atomic.Int32
	modelWaiting, modelStopped := make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		var payload struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			http.Error(w, "invalid", 400)
			return
		}
		if len(payload.Messages) > 0 && string(payload.Messages[len(payload.Messages)-1].Content) == `"cancel-through-python"` {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			close(modelWaiting)
			<-r.Context().Done()
			close(modelStopped)
			return
		}
		count := 0
		knowledge := ""
		for _, m := range payload.Messages {
			if m.Role == "tool" {
				count++
			}
		}
		for _, tool := range payload.Tools {
			if strings.HasPrefix(tool.Function.Name, "knowledge__") {
				knowledge = tool.Function.Name
			}
		}
		if knowledge == "" {
			t.Error("knowledge not advertised")
			http.Error(w, "missing", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b) }
		if count < 2 {
			name, args := "system_tools__echo_text", `{"text":"hello"}`
			if count == 1 {
				name, args = knowledge, `{"query":"MiniCoze"}`
			}
			send(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call_%d", count), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}}, "finish_reason": "tool_calls"}}})
		} else {
			last := string(payload.Messages[len(payload.Messages)-1].Content)
			if !strings.Contains(last, "chunkId") || !strings.Contains(last, "MiniCoze supports Go agents") {
				t.Error("model did not receive citations")
			}
			send(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "grounded answer"}, "finish_reason": "stop"}}})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	root, err := filepath.Abs("../../../agent-runtime-python")
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(root, ".venv", "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(root, ".venv", "Scripts", "python.exe")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-m", "uvicorn", "runtime.app:create_app", "--factory", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--log-level", "error")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "RUNTIME_SERVICE_TOKEN="+serviceToken, "MODEL_API_KEY=test-model-key", "MODEL_ALLOWLIST=test-model", "MODEL_BASE_URL="+provider.URL+"/v1", "GO_TOOL_GATEWAY_URL="+gateway.URL)
	logFile, err := os.CreateTemp(t.TempDir(), "runtime-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	ready := false
	client := &http.Client{Timeout: time.Second}
	for i := 0; i < 100; i++ {
		res, err := client.Get(origin + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		log, _ := os.ReadFile(logFile.Name())
		t.Fatalf("Python unavailable: %s", log)
	}
	probe, err := RuntimeReadiness(origin, serviceToken, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = probe(ctx); err != nil {
		t.Fatal("tool Runtime readiness", err)
	}
	if err = RegisterPersistentRuns(mux, auth, store, origin, serviceToken); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/api/agent-runs/stream", strings.NewReader(fmt.Sprintf(`{"agentId":"tool-agent","message":"MiniCoze","preview":%t}`, preview)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", "real-tool-chain"+suffix)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 || !strings.Contains(string(body), "event: run.completed\n") || strings.Contains(string(body), "event: run.failed\n") {
			t.Fatalf("chain failed: %s", body)
		}
		if attempt == 0 && strings.Count(string(body), "event: tool.call.completed\n") != 2 {
			t.Fatalf("tool events missing: %s", body)
		}
		run, err := store.GetRun(ctx, "u1", res.Header.Get("X-Run-Id"))
		if err != nil || run.OutputText == nil || *run.OutputText != "grounded answer" {
			t.Fatal("final persistence", err)
		}
		calls, err := store.ListToolCalls(ctx, "u1", run.ID)
		if err != nil || len(calls) != 2 {
			t.Fatal("tool audit", calls, err)
		}
		metrics, err := store.RunMetrics(ctx, "u1", run.ID)
		if err != nil || len(metrics.Rounds) != 3 || metrics.ReportedUsage != nil || metrics.UsageComplete {
			t.Fatal("round receipts", metrics, err)
		}
	}
	if modelCalls.Load() != 3 {
		t.Fatal("idempotent retry called model", modelCalls.Load())
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/api/agent-runs/stream", strings.NewReader(fmt.Sprintf(`{"agentId":"tool-agent","message":"cancel-through-python","preview":%t}`, preview)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "cancel-real-python"+suffix)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	id := res.Header.Get("X-Run-Id")
	select {
	case <-modelWaiting:
	case <-time.After(3 * time.Second):
		t.Fatal("model not waiting")
	}
	stopRequest, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/api/agent-runs/"+id+"/cancel", nil)
	stopRequest.Header.Set("Authorization", "Bearer "+token)
	stopped, err := http.DefaultClient.Do(stopRequest)
	if err != nil {
		t.Fatal(err)
	}
	stopped.Body.Close()
	if stopped.StatusCode != 200 {
		t.Fatal("cancel rejected")
	}
	output, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(output), "event: run.failed\n") != 1 {
		t.Fatalf("cancel terminal: %s", output)
	}
	select {
	case <-modelStopped:
	case <-time.After(3 * time.Second):
		t.Fatal("persisted cancellation did not reach provider")
	}
	run, err := store.GetRun(ctx, "u1", id)
	if err != nil || run.Status != "CANCELLED" || run.OutputText != nil {
		t.Fatal("cancel not persisted", err)
	}
}
