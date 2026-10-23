package platform

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
)

func respond(w http.ResponseWriter, status, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message, "data": data})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func validEmail(value string) bool {
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

func AuthHandler(auth *Auth) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", auth.Login)
	mux.Handle("GET /api/auth/profile", auth.Middleware(http.HandlerFunc(auth.Profile)))
	return mux
}

func CORS(next http.Handler, origins []string) http.Handler {
	allowed := map[string]bool{}
	for _, origin := range origins {
		allowed[origin] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		w.Header().Add("Vary", "Origin")
		if origin != "" {
			if !allowed[origin] {
				respond(w, 403, 40300, "Origin not allowed", nil)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Visitor-Session")
			w.Header().Set("Access-Control-Expose-Headers", "X-Run-Id, Location, Retry-After, Idempotency-Replayed")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}
