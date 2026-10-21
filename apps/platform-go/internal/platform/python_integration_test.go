package platform

import (
	"bufio"
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
	"testing"
	"time"
)

// Opt-in real Go -> Python -> HTTP provider test, with no paid model or DB.
func TestGoPythonHTTPIntegration(t *testing.T) {
	if os.Getenv("RUN_PYTHON_INTEGRATION") != "1" {
		t.Skip("set RUN_PYTHON_INTEGRATION=1 with Python venv installed")
	}
	root, err := filepath.Abs("../../../agent-runtime-python")
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(root, ".venv", "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(root, ".venv", "Scripts", "python.exe")
	}
	releaseModel := make(chan struct{})
	modelCancelled := make(chan struct{})
	defer func() {
		select {
		case <-releaseModel:
		default:
			close(releaseModel)
		}
	}()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-model-key" {
			t.Error("model credential not isolated")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好🌏\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		if input.Messages[len(input.Messages)-1].Content == "cancel" {
			<-r.Context().Done()
			close(modelCancelled)
			return
		}
		select {
		case <-releaseModel:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-m", "uvicorn", "runtime.app:create_app", "--factory", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--log-level", "error")
	cmd.Dir = root
	const serviceToken = "test-runtime-service-token-32-characters"
	cmd.Env = append(os.Environ(), "RUNTIME_SERVICE_TOKEN="+serviceToken, "MODEL_API_KEY=test-model-key", "MODEL_ALLOWLIST=test-model", "MODEL_BASE_URL="+provider.URL+"/v1")
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
	for i := 0; i < 80; i++ {
		res, err := client.Get(origin + "/healthz")
		if err == nil {
			res.Body.Close()
			ready = res.StatusCode == 200
			if ready {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		data, _ := os.ReadFile(logFile.Name())
		t.Fatalf("Python failed to start: %s", data)
	}
	probe, err := RuntimeReadiness(origin, serviceToken, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = probe(ctx); err != nil {
		t.Fatal("real Python readiness", err)
	}
	auth, users := setup(t)
	token, _ := auth.Sign(users.user)
	mux := AuthHandler(auth)
	if err = RegisterPythonPreview(mux, auth, previewFixture(), origin, serviceToken); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(mux)
	defer gateway.Close()
	request, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/api/agent-runs/preview/stream", strings.NewReader(`{"agentId":"a1","message":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("gateway %d: %s", res.StatusCode, body)
	}
	scanner := bufio.NewScanner(res.Body)
	var kinds []string
	seenDelta := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if err = json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &event); err != nil {
			t.Fatal(err)
		}
		kind := event["type"].(string)
		kinds = append(kinds, kind)
		if kind == "message.delta" {
			if event["content"] != "你好🌏" {
				t.Fatal("Unicode changed")
			}
			seenDelta = true
			close(releaseModel)
		}
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !seenDelta || strings.Join(kinds, ",") != "run.created,run.in_progress,message.delta,message.completed,run.completed,stream.done" {
		t.Fatalf("unexpected events: %v", kinds)
	}
	cancelCtx, stop := context.WithCancel(ctx)
	defer stop()
	cancelRequest, _ := http.NewRequestWithContext(cancelCtx, "POST", gateway.URL+"/api/agent-runs/preview/stream", strings.NewReader(`{"agentId":"a1","message":"cancel"}`))
	cancelRequest.Header.Set("Authorization", "Bearer "+token)
	cancelResponse, err := http.DefaultClient.Do(cancelRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelResponse.Body.Close()
	cancelScanner := bufio.NewScanner(cancelResponse.Body)
	for cancelScanner.Scan() {
		if strings.Contains(cancelScanner.Text(), "event: message.delta") {
			break
		}
	}
	stop()
	cancelResponse.Body.Close()
	select {
	case <-modelCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("client cancellation did not reach model HTTP request")
	}
}
