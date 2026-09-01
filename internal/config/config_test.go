package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/dhiazfathra/go-pr-review-worker/internal/config"
	"github.com/dhiazfathra/go-pr-review-worker/internal/reviewer"
)

func TestLoadRequiresAtLeastOneProvider(t *testing.T) {
	_, err := config.Load()
	if err == nil {
		t.Fatal("want error when no provider credentials are set")
	}

	if !strings.Contains(err.Error(), "no provider configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("PRW_GITHUB_TOKEN", "tok")
	t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "sec")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Addr != ":8080" || cfg.DBPath != "prw.db" {
		t.Fatalf("cfg = %+v", cfg)
	}

	if cfg.MaxCycles != 2 {
		t.Fatalf("MaxCycles = %d, want 2 — the review budget is part of the spec", cfg.MaxCycles)
	}

	if cfg.EngineTimeout != 10*time.Minute {
		t.Fatalf("EngineTimeout = %s", cfg.EngineTimeout)
	}

	if cfg.MinSeverity != reviewer.SeverityMinor {
		t.Fatalf("MinSeverity = %q", cfg.MinSeverity)
	}

	if strings.Join(cfg.ClaudeArgs, " ") != "--print --output-format text" {
		t.Fatalf("ClaudeArgs = %v", cfg.ClaudeArgs)
	}

	if !cfg.GitHubEnabled() || cfg.GitLabEnabled() {
		t.Fatalf("enabled flags: github=%v gitlab=%v", cfg.GitHubEnabled(), cfg.GitLabEnabled())
	}

	if !cfg.AnnounceBudgetExhausted {
		t.Fatal("AnnounceBudgetExhausted should default to true")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("PRW_GITLAB_TOKEN", "tok")
	t.Setenv("PRW_GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("PRW_ADDR", ":9000")
	t.Setenv("PRW_MAX_CYCLES", "3")
	t.Setenv("PRW_ENGINE_TIMEOUT", "45s")
	t.Setenv("PRW_MIN_SEVERITY", "critical")
	t.Setenv("PRW_ANNOUNCE_BUDGET_EXHAUSTED", "false")
	t.Setenv("PRW_OPENCODE_ARGS", "run --quiet")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Addr != ":9000" || cfg.MaxCycles != 3 || cfg.EngineTimeout != 45*time.Second {
		t.Fatalf("cfg = %+v", cfg)
	}

	if cfg.MinSeverity != reviewer.SeverityCritical {
		t.Fatalf("MinSeverity = %q", cfg.MinSeverity)
	}

	if cfg.AnnounceBudgetExhausted {
		t.Fatal("AnnounceBudgetExhausted override ignored")
	}

	if strings.Join(cfg.OpencodeArgs, " ") != "run --quiet" {
		t.Fatalf("OpencodeArgs = %v", cfg.OpencodeArgs)
	}

	if !cfg.GitLabEnabled() {
		t.Fatal("GitLabEnabled = false")
	}
}

func TestLoadRejectsUnknownSeverity(t *testing.T) {
	t.Setenv("PRW_GITHUB_TOKEN", "tok")
	t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "sec")
	t.Setenv("PRW_MIN_SEVERITY", "whatever")

	if _, err := config.Load(); err == nil {
		t.Fatal("want error for an unknown severity threshold")
	}
}

func TestUnparsableNumericValuesFallBackToDefaults(t *testing.T) {
	t.Setenv("PRW_GITHUB_TOKEN", "tok")
	t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "sec")
	t.Setenv("PRW_MAX_CYCLES", "not-a-number")
	t.Setenv("PRW_RETRY_DELAY", "not-a-duration")
	t.Setenv("PRW_ANNOUNCE_BUDGET_EXHAUSTED", "maybe")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.MaxCycles != 2 || cfg.RetryDelay != 30*time.Second || !cfg.AnnounceBudgetExhausted {
		t.Fatalf("cfg = %+v, want documented defaults on unparsable input", cfg)
	}
}

func TestPartialCredentialsAreNotEnabled(t *testing.T) {
	t.Setenv("PRW_GITHUB_TOKEN", "tok") // no webhook secret
	t.Setenv("PRW_GITLAB_WEBHOOK_SECRET", "sec")

	if _, err := config.Load(); err == nil {
		t.Fatal("want error: a token without a webhook secret cannot verify deliveries")
	}
}
