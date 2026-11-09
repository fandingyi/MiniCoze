package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Checks share one deadline; failures expose no dependency addresses or secrets.
func RegisterReadiness(mux *http.ServeMux, lifecycle context.Context, checks ...func(context.Context) error) {
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		stop := context.AfterFunc(lifecycle, cancel)
		defer stop()
		for _, check := range checks {
			if lifecycle.Err() != nil || check(ctx) != nil {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		if ctx.Err() != nil || lifecycle.Err() != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
}

func RuntimeReadiness(origin, token string, requireTools bool) (func(context.Context) error, error) {
	target, err := url.Parse(origin)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Path != "" && target.Path != "/") || len(token) < 32 {
		return nil, errors.New("invalid runtime readiness configuration")
	}
	target.Path = "/internal/v1/ready"
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return errors.New("runtime unavailable")
		}
		var status struct {
			Status     string `json:"status"`
			ToolsReady bool   `json:"tools_ready"`
		}
		if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&status) != nil || status.Status != "ready" || (requireTools && !status.ToolsReady) {
			return errors.New("runtime incompatible or not ready")
		}
		return nil
	}, nil
}
