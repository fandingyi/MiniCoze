package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"minicoze/platform/internal/platform"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	flags, err := readStartupFlags(os.Getenv)
	if err != nil {
		return err
	}
	dsn, err := platform.DatabaseURL(os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	ttl, err := platform.ParseTokenTTL(os.Getenv("JWT_EXPIRES_IN"))
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errors.New("database configuration invalid")
	}
	defer db.Close()
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = db.PingContext(ctx)
	cancel()
	if err != nil {
		return errors.New("database unavailable; verify DATABASE_URL and connectivity")
	}
	store := platform.Postgres{DB: db}
	auth, err := platform.NewAuth(store, os.Getenv("JWT_SECRET"), ttl)
	if err != nil {
		return err
	}
	lifecycle, stopLifecycle := context.WithCancel(context.Background())
	defer stopLifecycle()
	mux := platform.AuthHandler(auth)
	platform.RegisterCatalog(mux, auth, platform.Access{Store: store}, store)
	if origin := os.Getenv("PYTHON_RUNTIME_URL"); origin != "" {
		if err := platform.RegisterPythonPreview(mux, auth, store, origin, os.Getenv("RUNTIME_SERVICE_TOKEN")); err != nil {
			return err
		}
	}
	if flags.persisted {
		if err := platform.RegisterToolGateway(mux, auth, store, os.Getenv("RUNTIME_SERVICE_TOKEN")); err != nil {
			return err
		}
		options := platform.DefaultRunOptions()
		options.Lifecycle = lifecycle
		if value := os.Getenv("GO_RUN_TIMEOUT"); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return errors.New("invalid GO_RUN_TIMEOUT")
			}
			options.Timeout = parsed
		}
		if value := os.Getenv("GO_RUN_MAX_CONCURRENT"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return errors.New("invalid GO_RUN_MAX_CONCURRENT")
			}
			options.MaxConcurrent = parsed
		}
		platform.RegisterRunMetrics(mux, auth, store)
		if value := os.Getenv("GO_PUBLIC_MAX_CONCURRENT"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return errors.New("invalid GO_PUBLIC_MAX_CONCURRENT")
			}
			options.MaxPerChannel = parsed
		}
		if flags.public {
			if err := platform.RegisterPublicRoutes(mux, store, os.Getenv("PUBLIC_SESSION_SECRET"), flags.secure); err != nil {
				return err
			}
		}
		if err := platform.RegisterPersistentRunsWithOptions(mux, auth, store, os.Getenv("PYTHON_RUNTIME_URL"), os.Getenv("RUNTIME_SERVICE_TOKEN"), options); err != nil {
			return err
		}
		check, stopCheck := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := store.ExpireRuns(check)
		if err == nil {
			_, err = db.ExecContext(check, `SELECT "runId" FROM "AgentToolCall" LIMIT 0`)
		}
		if err == nil {
			_, err = db.ExecContext(check, `SELECT "runId" FROM "AgentModelRound" LIMIT 0`)
		}
		stopCheck()
		if err != nil {
			return errors.New("persistent run schema unavailable; apply Prisma migrations first")
		}
		go platform.RunReaper(lifecycle, store, 15*time.Second, slog.Default())
	} else if upstream := os.Getenv("NESTJS_INTERNAL_URL"); upstream != "" {
		if err := platform.RegisterStream(mux, auth, platform.Access{Store: store}, upstream); err != nil {
			return err
		}
	}
	checks := []func(context.Context) error{db.PingContext}
	if flags.persisted {
		checks = append(checks, func(ctx context.Context) error {
			_, err := db.ExecContext(ctx, `SELECT r."id",t."runId",m."runId" FROM "AgentRun" r LEFT JOIN "AgentToolCall" t ON false LEFT JOIN "AgentModelRound" m ON false LIMIT 0`)
			return err
		})
	}
	if origin := os.Getenv("PYTHON_RUNTIME_URL"); origin != "" {
		check, err := platform.RuntimeReadiness(origin, os.Getenv("RUNTIME_SERVICE_TOKEN"), flags.persisted)
		if err != nil {
			return err
		}
		checks = append(checks, check)
	}
	platform.RegisterReadiness(mux, lifecycle, checks...)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	origins := strings.Split(env("CORS_ORIGIN", "http://localhost:5173,http://127.0.0.1:5173"), ",")
	for i := range origins {
		origins[i] = strings.TrimSpace(origins[i])
	}
	server := &http.Server{Addr: env("GO_ADDR", "127.0.0.1:3001"), Handler: platform.CORS(mux, origins), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	stop, stopCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopCancel()
	done := make(chan error, 1)
	go func() { log.Printf("Go platform listening on %s", server.Addr); done <- server.ListenAndServe() }()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-stop.Done():
		stopLifecycle()
		slog.Info("server.draining")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
