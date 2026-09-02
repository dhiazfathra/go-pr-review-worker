// Package config loads runtime settings from the environment. Environment
// variables only: the deployment target is a single small VM with a systemd
// unit, and a config file format would be one more thing to validate.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dhiazfathra/go-pr-review-worker/internal/reviewer"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Addr   string
	DBPath string

	GitHubToken   string
	GitHubAPI     string
	GitHubSecret  string
	GitLabToken   string
	GitLabAPI     string
	GitLabSecret  string
	ClaudeBinary  string
	ClaudeArgs    []string
	ClaudeModel   string
	OpencodeBin   string
	OpencodeArgs  []string
	EngineTimeout time.Duration

	MaxCycles               int
	MaxAttempts             int
	MaxComments             int
	MaxFindings             int
	MinSeverity             reviewer.Severity
	RetryDelay              time.Duration
	PollInterval            time.Duration
	AnnounceBudgetExhausted bool
	LogLevel                string

	// WatchRepos are "provider:owner/name" entries the watcher polls for pushes
	// a webhook never delivered. Empty disables the watcher entirely.
	WatchRepos []WatchRepo
	// WatchInterval is how often the watcher re-lists open pull requests.
	WatchInterval time.Duration

	// VerifyReplies turns on the follow-up pass: re-checking the worker's own
	// unresolved threads against the new commits and the author's replies.
	VerifyReplies bool
	// ApproveWhenResolved lets the worker submit an APPROVE review once every
	// thread it opened is resolved. Off by default: approving is an act with
	// merge consequences, so it stays an explicit operator decision.
	ApproveWhenResolved bool
}

// WatchRepo is one repository the watcher polls.
type WatchRepo struct {
	Provider string
	Repo     string
}

// Load reads the environment and applies defaults. It fails when no provider
// is usable, because a worker with no forge credentials can only ever no-op.
func Load() (Config, error) {
	cfg := Config{
		Addr:                    env("PRW_ADDR", ":8080"),
		DBPath:                  env("PRW_DB", "prw.db"),
		GitHubToken:             os.Getenv("PRW_GITHUB_TOKEN"),
		GitHubAPI:               env("PRW_GITHUB_API", "https://api.github.com"),
		GitHubSecret:            os.Getenv("PRW_GITHUB_WEBHOOK_SECRET"),
		GitLabToken:             os.Getenv("PRW_GITLAB_TOKEN"),
		GitLabAPI:               env("PRW_GITLAB_API", "https://gitlab.com/api/v4"),
		GitLabSecret:            os.Getenv("PRW_GITLAB_WEBHOOK_SECRET"),
		ClaudeBinary:            env("PRW_CLAUDE_BIN", "claude"),
		ClaudeModel:             os.Getenv("PRW_CLAUDE_MODEL"),
		ClaudeArgs:              fields("PRW_CLAUDE_ARGS", "--print --output-format text"),
		OpencodeBin:             env("PRW_OPENCODE_BIN", "opencode"),
		OpencodeArgs:            fields("PRW_OPENCODE_ARGS", "run"),
		EngineTimeout:           duration("PRW_ENGINE_TIMEOUT", 10*time.Minute),
		MaxCycles:               integer("PRW_MAX_CYCLES", 2),
		MaxAttempts:             integer("PRW_MAX_ATTEMPTS", 3),
		MaxComments:             integer("PRW_MAX_COMMENTS", 20),
		MaxFindings:             integer("PRW_MAX_FINDINGS", 25),
		MinSeverity:             reviewer.Severity(env("PRW_MIN_SEVERITY", string(reviewer.SeverityMinor))),
		RetryDelay:              duration("PRW_RETRY_DELAY", 30*time.Second),
		PollInterval:            duration("PRW_POLL_INTERVAL", 30*time.Second),
		AnnounceBudgetExhausted: boolean("PRW_ANNOUNCE_BUDGET_EXHAUSTED", true),
		LogLevel:                env("PRW_LOG_LEVEL", "info"),
		WatchInterval:           duration("PRW_WATCH_INTERVAL", 2*time.Minute),
		VerifyReplies:           boolean("PRW_VERIFY_REPLIES", true),
		ApproveWhenResolved:     boolean("PRW_APPROVE_WHEN_RESOLVED", false),
	}

	watched, err := watchRepos(os.Getenv("PRW_WATCH_REPOS"))
	if err != nil {
		return Config{}, err
	}

	cfg.WatchRepos = watched

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) validate() error {
	githubReady := c.GitHubToken != "" && c.GitHubSecret != ""
	gitlabReady := c.GitLabToken != "" && c.GitLabSecret != ""

	if !githubReady && !gitlabReady {
		return errors.New(
			"no provider configured: set PRW_GITHUB_TOKEN + PRW_GITHUB_WEBHOOK_SECRET " +
				"and/or PRW_GITLAB_TOKEN + PRW_GITLAB_WEBHOOK_SECRET",
		)
	}

	if _, ok := map[reviewer.Severity]bool{
		reviewer.SeverityCritical: true,
		reviewer.SeverityMajor:    true,
		reviewer.SeverityMinor:    true,
		reviewer.SeverityNit:      true,
	}[c.MinSeverity]; !ok {
		return fmt.Errorf("PRW_MIN_SEVERITY %q is not one of critical|major|minor|nit", c.MinSeverity)
	}

	for name, v := range map[string]int{
		"PRW_MAX_CYCLES":   c.MaxCycles,
		"PRW_MAX_ATTEMPTS": c.MaxAttempts,
		"PRW_MAX_COMMENTS": c.MaxComments,
		"PRW_MAX_FINDINGS": c.MaxFindings,
	} {
		if v <= 0 {
			return fmt.Errorf("%s must be positive, got %d", name, v)
		}
	}

	for name, v := range map[string]time.Duration{
		"PRW_ENGINE_TIMEOUT": c.EngineTimeout,
		"PRW_RETRY_DELAY":    c.RetryDelay,
		"PRW_POLL_INTERVAL":  c.PollInterval,
		"PRW_WATCH_INTERVAL": c.WatchInterval,
	} {
		if v <= 0 {
			return fmt.Errorf("%s must be positive, got %s", name, v)
		}
	}

	// Watching a forge whose credentials are missing would poll forever and
	// fail every time; that is a startup misconfiguration, not a runtime event.
	for _, wr := range c.WatchRepos {
		if wr.Provider == "github" && !c.GitHubEnabled() {
			return fmt.Errorf(
				"PRW_WATCH_REPOS watches github:%s but GitHub is not configured "+
					"(needs PRW_GITHUB_TOKEN + PRW_GITHUB_WEBHOOK_SECRET)", wr.Repo)
		}

		if wr.Provider == "gitlab" && !c.GitLabEnabled() {
			return fmt.Errorf(
				"PRW_WATCH_REPOS watches gitlab:%s but GitLab is not configured "+
					"(needs PRW_GITLAB_TOKEN + PRW_GITLAB_WEBHOOK_SECRET)", wr.Repo)
		}
	}

	for name, v := range map[string]string{"PRW_GITHUB_API": c.GitHubAPI, "PRW_GITLAB_API": c.GitLabAPI} {
		if err := requireSecureURL(name, v); err != nil {
			return err
		}
	}

	return nil
}

// watchRepos parses PRW_WATCH_REPOS: a comma- or space-separated list of
// "provider:owner/name" entries. The provider prefix is required rather than
// inferred, because the same "owner/name" is a valid path on both forges and
// guessing would poll the wrong API with the wrong token.
func watchRepos(raw string) ([]WatchRepo, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})

	out := make([]WatchRepo, 0, len(fields))
	seen := make(map[string]bool, len(fields))

	for _, f := range fields {
		provider, repo, ok := strings.Cut(f, ":")
		if !ok {
			return nil, fmt.Errorf(
				"PRW_WATCH_REPOS entry %q must be \"provider:owner/name\", e.g. github:octocat/hello", f)
		}

		provider = strings.ToLower(strings.TrimSpace(provider))
		repo = strings.Trim(strings.TrimSpace(repo), "/")

		if provider != "github" && provider != "gitlab" {
			return nil, fmt.Errorf("PRW_WATCH_REPOS entry %q: provider must be github or gitlab", f)
		}

		if repo == "" || !strings.Contains(repo, "/") {
			return nil, fmt.Errorf("PRW_WATCH_REPOS entry %q: repository must be owner/name", f)
		}

		key := provider + ":" + repo
		if seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, WatchRepo{Provider: provider, Repo: repo})
	}

	return out, nil
}

// requireSecureURL rejects a forge API endpoint that would send an
// authenticated request over plaintext. Every request carries a forge token,
// so the scheme is a credential-confidentiality decision, not a preference.
func requireSecureURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q: %w", name, raw, err)
	}

	// "https:host", "https:" and "https:///path" all parse with the right
	// scheme and an empty host, which would otherwise pass as secure and then
	// fail later as an unusable relative URL.
	if u.Hostname() == "" {
		return fmt.Errorf("%s %q has no host", name, raw)
	}

	if u.Scheme == "https" {
		return nil
	}

	// A loopback endpoint never leaves the machine, so plaintext there is a
	// local-fixture concern rather than an exposed credential — but it stays an
	// explicit opt-in, so a typo in a real deployment cannot silently downgrade.
	loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme == "http" && loopback && boolean("PRW_ALLOW_INSECURE_LOOPBACK", false) {
		return nil
	}

	if u.Scheme == "http" && loopback {
		return fmt.Errorf(
			"%s %q is plaintext: set PRW_ALLOW_INSECURE_LOOPBACK=true to allow a loopback endpoint", name, raw)
	}

	return fmt.Errorf("%s %q must use https", name, raw)
}

// GitHubEnabled reports whether GitHub webhooks can be served.
func (c Config) GitHubEnabled() bool {
	return c.GitHubToken != "" && c.GitHubSecret != ""
}

// GitLabEnabled reports whether GitLab webhooks can be served.
func (c Config) GitLabEnabled() bool {
	return c.GitLabToken != "" && c.GitLabSecret != ""
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}

	return fallback
}

func fields(key, fallback string) []string {
	return strings.Fields(env(key, fallback))
}

func integer(key string, fallback int) int {
	v, err := strconv.Atoi(env(key, ""))
	if err != nil {
		return fallback
	}

	return v
}

func duration(key string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(env(key, ""))
	if err != nil {
		return fallback
	}

	return v
}

func boolean(key string, fallback bool) bool {
	v, err := strconv.ParseBool(env(key, ""))
	if err != nil {
		return fallback
	}

	return v
}
