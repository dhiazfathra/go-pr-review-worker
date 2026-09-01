// Command pr-review-worker serves forge webhooks and reviews pull requests one
// at a time by driving an agentic coding CLI in headless mode.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dhiazfathra/go-pr-review-worker/internal/config"
	"github.com/dhiazfathra/go-pr-review-worker/internal/provider"
	"github.com/dhiazfathra/go-pr-review-worker/internal/reviewer"
	"github.com/dhiazfathra/go-pr-review-worker/internal/store"
	"github.com/dhiazfathra/go-pr-review-worker/internal/webhook"
	"github.com/dhiazfathra/go-pr-review-worker/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "pr-review-worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// Buffered by one: the channel is only a wake-up hint, the queue of record
	// is SQLite, so a dropped hint costs at most one poll interval.
	notify := make(chan struct{}, 1)

	providers := map[string]provider.Provider{}
	if cfg.GitHubEnabled() {
		providers["github"] = provider.NewGitHub(cfg.GitHubAPI, cfg.GitHubToken)
	}

	if cfg.GitLabEnabled() {
		providers["gitlab"] = provider.NewGitLab(cfg.GitLabAPI, cfg.GitLabToken)
	}

	engine := reviewer.Chain{Engines: []reviewer.Engine{
		cliEngine("claude", cfg.ClaudeBinary, cfg.ClaudeArgs, cfg, log),
		cliEngine("opencode", cfg.OpencodeBin, cfg.OpencodeArgs, cfg, log),
	}}

	w := worker.New(st, engine, providers, notify, worker.Config{
		MaxCycles:               cfg.MaxCycles,
		MaxAttempts:             cfg.MaxAttempts,
		RetryDelay:              cfg.RetryDelay,
		PollInterval:            cfg.PollInterval,
		MinSeverity:             cfg.MinSeverity,
		MaxComments:             cfg.MaxComments,
		AnnounceBudgetExhausted: cfg.AnnounceBudgetExhausted,
	}, log)

	handler := &webhook.Handler{
		Store:        st,
		Notify:       notify,
		GitHubSecret: cfg.GitHubSecret,
		GitLabSecret: cfg.GitLabSecret,
		Logger:       log,
	}

	mux := http.NewServeMux()
	handler.Routes(mux)
	mux.HandleFunc("GET /healthz", healthz(st))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	done := make(chan struct{})

	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("http shutdown failed", "error", err)
		}
	}()

	log.Info(
		"listening",
		"addr", cfg.Addr,
		"db", cfg.DBPath,
		"providers", strings.Join(providerNames(providers), ","),
		"max_cycles", cfg.MaxCycles,
	)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}

	<-done
	log.Info("stopped")

	return nil
}

func providerNames(providers map[string]provider.Provider) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}

	return names
}

func cliEngine(
	name, binary string,
	args []string,
	cfg config.Config,
	log *slog.Logger,
) reviewer.Engine {
	return reviewer.CLI{
		EngineName:     name,
		Binary:         binary,
		Args:           args,
		Timeout:        cfg.EngineTimeout,
		MaxFindings:    cfg.MaxFindings,
		MaxOutputBytes: 4 << 20,
		Logger:         log,
	}
}

func healthz(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pending, err := st.PendingCount(r.Context())
		if err != nil {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		// A failed write to a disconnected health checker is not actionable.
		_, _ = fmt.Fprintf(w, `{"status":"ok","pending_jobs":%d}`, pending)
	}
}

func newLogger(level string) *slog.Logger {
	lvl := slog.LevelInfo

	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}

	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
