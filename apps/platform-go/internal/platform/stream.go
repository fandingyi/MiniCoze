package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// Temporary migration adapter: Go owns ingress authentication; NestJS still
// owns execution and persistence until the Python and Run migrations land.
func RegisterStream(mux *http.ServeMux, auth *Auth, access Access, upstream string) error {
	target, err := url.Parse(upstream)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil || (target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.Fragment != "" {
		return errors.New("NESTJS_INTERNAL_URL must be a trusted HTTP(S) origin")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 100, MaxIdleConnsPerHost: 20, MaxConnsPerHost: 100}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/api/agent-runs/stream"
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = ""
			pr.Out.Header.Del("X-User-Id")
			pr.Out.Header.Del("Cookie")
		},
		Transport: transport, FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			response.Header.Del("Access-Control-Allow-Origin")
			response.Header.Del("Access-Control-Allow-Credentials")
			response.Header.Set("X-Accel-Buffering", "no")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			respond(w, 502, 50000, "运行服务暂不可用", nil)
		},
	}
	// Bounded admission: no in-memory waiting queue during this migration step.
	slots := make(chan struct{}, 32)
	mux.Handle("POST /api/agent-runs/stream", auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			AgentID         string            `json:"agentId"`
			Message         *string           `json:"message"`
			ConversationID  string            `json:"conversationId,omitempty"`
			Preview         bool              `json:"preview,omitempty"`
			Model           *string           `json:"model,omitempty"`
			SystemPrompt    *string           `json:"systemPrompt,omitempty"`
			Temperature     *float64          `json:"temperature,omitempty"`
			MaxTokens       *float64          `json:"maxTokens,omitempty"`
			Tools           []json.RawMessage `json:"tools,omitempty"`
			KnowledgeBaseID *string           `json:"knowledgeBaseId,omitempty"`
		}
		if decode(w, r, &input) != nil || input.AgentID == "" || input.Message == nil || (input.Temperature != nil && *input.Temperature < 0) || (input.MaxTokens != nil && *input.MaxTokens < 1) {
			respond(w, 400, 40000, "运行参数错误", nil)
			return
		}
		user := r.Context().Value(userKey{}).(User)
		checkCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		err := access.Run(checkCtx, user.ID, input.AgentID, input.ConversationID, input.Preview)
		cancel()
		if errors.Is(err, ErrForbidden) {
			respond(w, 403, 40300, "无权访问该资源", nil)
			return
		}
		if errors.Is(err, ErrUnpublished) {
			respond(w, 400, 40000, "Agent 尚未发布或已下线", nil)
			return
		}
		if err != nil {
			respond(w, 503, 50000, "权限服务暂不可用", nil)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			respond(w, 429, 42900, "运行并发已满，请稍后重试", nil)
			return
		}
		body, err := json.Marshal(input)
		if err != nil {
			respond(w, 400, 40000, "运行参数错误", nil)
			return
		}
		// Do not replace SSE event IDs or serialize frames again at this bridge.
		ctx, stop := context.WithTimeout(r.Context(), 120*time.Second)
		defer stop()
		request := r.Clone(ctx)
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
		request.Header.Set("Content-Type", "application/json")
		defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
		proxy.ServeHTTP(&streamWriter{ResponseWriter: w}, request)
	})))
	return nil
}

// ReverseProxy copies with a fixed buffer; each downstream write gets a bounded
// deadline so a slow browser cannot keep a slot forever.
type streamWriter struct{ http.ResponseWriter }

func (w *streamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *streamWriter) Write(data []byte) (int, error) {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(10 * time.Second))
	return w.ResponseWriter.Write(data)
}
func (w *streamWriter) Flush() {
	_ = w.FlushError()
}
func (w *streamWriter) FlushError() error {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(10 * time.Second))
	return http.NewResponseController(w.ResponseWriter).Flush()
}
