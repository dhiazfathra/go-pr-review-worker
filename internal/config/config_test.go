package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/dhiazfathra/go-pr-review-worker/internal/config"
	"github.com/dhiazfathra/go-pr-review-worker/internal/reviewer"
)

// prwVars lists every PRW_* variable config.Load reads. clearEnv sets each to
// empty (config.Load treats "" the same as unset), via t.Setenv so the real
// value — if a developer or CI runner happens to export one — is restored
// after the test instead of leaking into the next one.
var prwVars = []string{
	"PRW_ADDR", "PRW_DB",
	"PRW_GITHUB_TOKEN", "PRW_GITHUB_API", "PRW_GITHUB_WEBHOOK_SECRET",
	"PRW_GITLAB_TOKEN", "PRW_GITLAB_API", "PRW_GITLAB_WEBHOOK_SECRET",
	"PRW_CLAUDE_BIN", "PRW_CLAUDE_ARGS", "PRW_OPENCODE_BIN", "PRW_OPENCODE_ARGS",
	"PRW_ENGINE_TIMEOUT", "PRW_MAX_CYCLES", "PRW_MAX_ATTEMPTS", "PRW_MAX_COMMENTS",
	"PRW_MAX_FINDINGS", "PRW_MIN_SEVERITY", "PRW_RETRY_DELAY", "PRW_POLL_INTERVAL",
	"PRW_ANNOUNCE_BUDGET_EXHAUSTED", "PRW_LOG_LEVEL",
	"PRW_ALLOW_INSECURE_LOOPBACK",
	"PRW_CLAUDE_MODEL", "PRW_WATCH_REPOS", "PRW_WATCH_INTERVAL",
	"PRW_VERIFY_REPLIES", "PRW_APPROVE_WHEN_RESOLVED",
}

func clearEnv(t *testing.T) {
	t.Helper()

	for _, k := range prwVars {
		t.Setenv(k, "")
	}
}

func TestLoadRequiresAtLeastOneProvider(t *testing.T) {
	clearEnv(t)

	_, err := config.Load()
	if err == nil {
		t.Fatal("want error when no provider credentials are set")
	}

	if !strings.Contains(err.Error(), "no provider configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
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
	clearEnv(t)
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
	clearEnv(t)
	t.Setenv("PRW_GITHUB_TOKEN", "tok")
	t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "sec")
	t.Setenv("PRW_MIN_SEVERITY", "whatever")

	if _, err := config.Load(); err == nil {
		t.Fatal("want error for an unknown severity threshold")
	}
}

func TestUnparsableNumericValuesFallBackToDefaults(t *testing.T) {
	clearEnv(t)
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
	clearEnv(t)
	t.Setenv("PRW_GITHUB_TOKEN", "tok") // no webhook secret
	t.Setenv("PRW_GITLAB_WEBHOOK_SECRET", "sec")

	if _, err := config.Load(); err == nil {
		t.Fatal("want error: a token without a webhook secret cannot verify deliveries")
	}
}

// Every request to the forge carries a token, so the endpoint's scheme decides
// whether that token can end up on the wire in cleartext.
func TestForgeAPIEndpointMustNotExposeTheToken(t *testing.T) {
	cases := []struct {
		name, api string
		loopback  string // PRW_ALLOW_INSECURE_LOOPBACK
		wantErr   bool
	}{
		{name: "https", api: "https://api.github.com"},
		{name: "https with port", api: "https://ghe.example.com:8443/api/v3"},

		{name: "plaintext", api: "http://api.example.com", wantErr: true},
		{name: "plaintext loopback without opt-in", api: "http://127.0.0.1:9000", wantErr: true},
		{name: "plaintext loopback with opt-in", api: "http://127.0.0.1:9000", loopback: "true"},
		{name: "localhost with opt-in", api: "http://localhost:9000", loopback: "true"},
		// The opt-in covers loopback only; it must not wave through a real host.
		{name: "opt-in does not allow a remote host", api: "http://evil.example.com", loopback: "true", wantErr: true},

		// url.Parse accepts these with the right scheme and no host at all.
		{name: "https with no host", api: "https:forge.example", wantErr: true},
		{name: "bare https scheme", api: "https:", wantErr: true},
		{name: "https empty authority", api: "https:///api/v3", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("PRW_GITHUB_TOKEN", "tok")
			t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "sec")
			t.Setenv("PRW_GITHUB_API", c.api)

			if c.loopback != "" {
				t.Setenv("PRW_ALLOW_INSECURE_LOOPBACK", c.loopback)
			}

			_, err := config.Load()
			if c.wantErr && err == nil {
				t.Fatalf("PRW_GITHUB_API=%q was accepted; the forge token would travel in cleartext", c.api)
			}

			if !c.wantErr && err != nil {
				t.Fatalf("PRW_GITHUB_API=%q rejected: %v", c.api, err)
			}
		})
	}
}

func TestWatchReposParsesProviderPrefixedEntries(t *testing.T) {
	clearEnv(t)
	t.Setenv("PRW_GITHUB_TOKEN", "t")
	t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "s")
	t.Setenv("PRW_WATCH_REPOS", "github:octocat/hello, github:octocat/hello ,github:o/two")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.WatchRepos) != 2 {
		t.Fatalf("WatchRepos = %+v, want the duplicate collapsed", cfg.WatchRepos)
	}

	if cfg.WatchRepos[0] != (config.WatchRepo{Provider: "github", Repo: "octocat/hello"}) {
		t.Errorf("WatchRepos[0] = %+v", cfg.WatchRepos[0])
	}
}

func TestWatchReposRejectsMalformedEntries(t *testing.T) {
	for _, raw := range []string{
		"octocat/hello",       // no provider prefix
		"bitbucket:o/r",       // unknown provider
		"github:justtheowner", // not owner/name
	} {
		t.Run(raw, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("PRW_GITHUB_TOKEN", "t")
			t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "s")
			t.Setenv("PRW_WATCH_REPOS", raw)

			if _, err := config.Load(); err == nil {
				t.Fatalf("Load() accepted %q; a typo here means the worker polls nothing and says nothing", raw)
			}
		})
	}
}

// Watching a forge with no credentials could only ever fail on every poll, so
// it is refused at startup rather than logged forever.
func TestWatchReposRequiresTheForgeToBeConfigured(t *testing.T) {
	clearEnv(t)
	t.Setenv("PRW_GITHUB_TOKEN", "t")
	t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "s")
	t.Setenv("PRW_WATCH_REPOS", "gitlab:group/project")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() accepted a gitlab watch target with no gitlab credentials")
	}

	if !strings.Contains(err.Error(), "GitLab is not configured") {
		t.Errorf("error = %v, want it to name the missing credentials", err)
	}
}

func TestVerifyDefaultsOnAndApprovalDefaultsOff(t *testing.T) {
	clearEnv(t)
	t.Setenv("PRW_GITHUB_TOKEN", "t")
	t.Setenv("PRW_GITHUB_WEBHOOK_SECRET", "s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.VerifyReplies {
		t.Error("VerifyReplies defaults off; answering the author should not need opting in")
	}

	if cfg.ApproveWhenResolved {
		t.Error("ApproveWhenResolved defaults on; approving has merge consequences and must be opt-in")
	}

	if cfg.WatchInterval != 2*time.Minute {
		t.Errorf("WatchInterval = %s, want 2m", cfg.WatchInterval)
	}
}
