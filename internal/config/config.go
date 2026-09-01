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
	}

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
	} {
		if v <= 0 {
			return fmt.Errorf("%s must be positive, got %s", name, v)
		}
	}

	for name, v := range map[string]string{"PRW_GITHUB_API": c.GitHubAPI, "PRW_GITLAB_API": c.GitLabAPI} {
		if err := requireSecureURL(name, v); err != nil {
			return err
		}
	}

	return nil
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
