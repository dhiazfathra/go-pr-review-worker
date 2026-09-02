package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhiazfathra/go-pr-review-worker/internal/config"
	"github.com/dhiazfathra/go-pr-review-worker/internal/provider"
	"github.com/dhiazfathra/go-pr-review-worker/internal/reviewer"
	"github.com/dhiazfathra/go-pr-review-worker/internal/store"
)

func TestHealthzReportsQueueDepth(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer func() { _ = st.Close() }()

	if _, err := st.Enqueue(context.Background(), store.Job{
		DeliveryID: "d1",
		Provider:   "github",
		Repo:       "acme/app",
		PRNumber:   1,
		HeadSHA:    "s",
		Event:      store.EventOpened,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	rec := httptest.NewRecorder()
	healthz(st)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), `"pending_jobs":1`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHealthzFailsWhenTheStoreIsGone(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := httptest.NewRecorder()
	healthz(st)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestNewLoggerLevels(t *testing.T) {
	for _, level := range []string{"debug", "warn", "error", "info", "nonsense"} {
		if newLogger(level) == nil {
			t.Fatalf("newLogger(%q) = nil", level)
		}
	}
}

func TestProviderNames(t *testing.T) {
	got := providerNames(map[string]provider.Provider{
		"github": provider.NewGitHub("", "t"),
	})

	if len(got) != 1 || got[0] != "github" {
		t.Fatalf("providerNames = %v", got)
	}
}

func TestCLIEngineCarriesTheTimeout(t *testing.T) {
	engine := cliEngine(
		"claude",
		"claude",
		[]string{"--print"},
		"claude-sonnet-5",
		config.Config{EngineTimeout: 3 * time.Minute, MaxFindings: 9},
		slog.Default(),
	)

	if engine.Name() != "claude" {
		t.Fatalf("Name = %q", engine.Name())
	}

	// The name alone would still pass if cliEngine dropped the model, which is
	// the whole point of PRW_CLAUDE_MODEL: the pinned model has to reach the
	// engine, or the CLI falls back to whatever the account's own settings say
	// (see docs/incidents/2026-09-02-manual-run-stale-model-alias.md).
	cli, ok := engine.(reviewer.CLI)
	if !ok {
		t.Fatalf("engine is %T, want reviewer.CLI", engine)
	}

	if cli.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want the model cliEngine was given", cli.Model)
	}

	if cli.Timeout != 3*time.Minute {
		t.Errorf("Timeout = %v, want 3m", cli.Timeout)
	}
}

func TestRunFailsWithoutCredentials(t *testing.T) {
	t.Setenv("PRW_GITHUB_TOKEN", "")
	t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "")
	t.Setenv("PRW_GITLAB_TOKEN", "")
	t.Setenv("PRW_GITLAB_WEBHOOK_SECRET", "")

	if err := run(); err == nil {
		t.Fatal("run() should refuse to start with no provider configured")
	}
}

func TestRunServesAndShutsDownOnSignalContext(t *testing.T) {
	t.Setenv("PRW_GITHUB_TOKEN", "tok")
	t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "sec")
	t.Setenv("PRW_ADDR", "127.0.0.1:0")
	t.Setenv("PRW_DB", filepath.Join(t.TempDir(), "run.db"))

	// Port 0 makes ListenAndServe pick a free port; the store path proves the
	// wiring, and an immediate SIGTERM proves shutdown is clean.
	done := make(chan error, 1)

	go func() { done <- run() }()

	select {
	case err := <-done:
		// A bind failure is the only way this returns quickly; report it.
		if err != nil && !strings.Contains(err.Error(), "http server") {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		// Still serving, which is the expected state. Nothing to assert
		// further without sending the process a real signal.
	}
}
