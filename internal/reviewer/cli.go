package reviewer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// rateLimitSignals are the substrings and patterns that mean "this engine is
// out of quota right now, try the other one". Matched case-insensitively
// against combined stdout+stderr of a failed invocation.
//
// Sources: Claude Code prints "Claude usage limit reached" / "5-hour limit
// reached" on quota exhaustion and surfaces the API's 429 and 529
// (overloaded_error) as text; OpenCode surfaces provider errors verbatim.
var rateLimitSignals = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`usage limit reached`,
	`limit reached[^\n]*resets`,
	`rate[ _-]?limit`,
	`rate[ _-]?limited`,
	`too many requests`,
	`\b429\b`,
	`\b529\b`,
	`overloaded_error`,
	`quota (?:exceeded|exhausted)`,
	`insufficient[ _-]quota`,
	`retry[ _-]after`,
}, "|"))

// defaultTimeout applies when CLI.Timeout is unset, so a zero value never
// produces an already-expired context.
const defaultTimeout = 10 * time.Minute

// childEnvAllowlist are the variables the engine process needs. Everything
// else in the worker's environment — forge tokens, webhook secrets — stays
// out, since contributor-controlled PR content reaches the engine and could
// otherwise exfiltrate them through its tools.
//
// USER and LOGNAME are not decoration: a CLI authenticated by subscription
// login rather than an API key reads its credentials from the OS keyring
// (macOS Keychain, libsecret), and that lookup is keyed by the account name.
// Drop them and the engine fails with "Invalid API key · Please run /login"
// on a host where `claude` works fine interactively.
var childEnvAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR",
	"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_MODEL",
}

// childEnv builds a minimal environment for the engine subprocess.
func childEnv() []string {
	env := []string{"CI=1"}

	for _, k := range childEnvAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}

	return env
}

// CLI runs an agentic coding CLI in headless mode as a review engine.
type CLI struct {
	// EngineName is the adapter name used in logs and the summary footer.
	EngineName string
	// Binary is the executable, resolved on PATH.
	Binary string
	// Args are the headless-mode flags. The prompt always goes on stdin, so a
	// megabyte-scale diff never hits the ARG_MAX limit.
	Args []string
	// Timeout bounds one invocation. A hung CLI would otherwise block the
	// single worker forever, so this is the liveness guarantee of the queue.
	Timeout time.Duration
	// MaxFindings caps how many comments one pass may produce.
	MaxFindings int
	// MaxOutputBytes caps captured output so a runaway CLI cannot exhaust RAM
	// on a 2 vCPU / 4 GB box.
	MaxOutputBytes int64
	Logger         *slog.Logger
}

// Name implements Engine.
func (c CLI) Name() string {
	return c.EngineName
}

// Review implements Engine: it spawns the CLI, feeds the prompt on stdin, and
// parses the JSON object the prompt contract demands.
func (c CLI) Review(ctx context.Context, req Request) (Result, error) {
	prompt := buildPrompt(req, c.MaxFindings)

	stdout, stderr, err := c.run(ctx, prompt)
	combined := stdout + "\n" + stderr

	if err != nil {
		if rateLimitSignals.MatchString(combined) {
			return Result{}, fmt.Errorf("%s: %w: %s", c.EngineName, ErrRateLimited, firstLine(combined))
		}

		return Result{}, fmt.Errorf("%s invocation: %w: %s", c.EngineName, err, firstLine(combined))
	}

	// A zero exit code with a rate-limit notice in the text still means no
	// review happened — some CLIs report quota problems as a normal message.
	if rateLimitSignals.MatchString(combined) && !strings.Contains(stdout, `"findings"`) {
		return Result{}, fmt.Errorf("%s: %w: %s", c.EngineName, ErrRateLimited, firstLine(combined))
	}

	res, err := parseResult(stdout)
	if err != nil {
		return Result{}, fmt.Errorf("%s output: %w", c.EngineName, err)
	}

	return res, nil
}

// run spawns the CLI in its own process group and returns its output. On
// timeout the whole group is killed, so a CLI that spawned children (a shell,
// a language server) cannot leak processes onto a small VM.
func (c CLI) run(ctx context.Context, prompt string) (string, string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(c.Binary, c.Args...) // nosemgrep: dangerous-exec-command -- Binary/Args come from operator-set PRW_*_BIN/PRW_*_ARGS config (internal/config), never from PR/webhook content
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = childEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, remaining: c.MaxOutputBytes}
	cmd.Stderr = &limitedWriter{w: &stderr, remaining: c.MaxOutputBytes}

	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("starting %s: %w", c.Binary, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return stdout.String(), stderr.String(), fmt.Errorf("%s exited: %w", c.Binary, err)
		}

		return stdout.String(), stderr.String(), nil

	case <-ctx.Done():
		if !c.kill(cmd, done) {
			<-done // reap, the group is already signalled
		}

		return stdout.String(), stderr.String(),
			fmt.Errorf("%s timed out after %s: %w", c.Binary, timeout, ctx.Err())
	}
}

// kill signals the whole process group, then SIGKILLs it after a short grace
// period so a CLI ignoring SIGTERM still cannot hold the worker. It reports
// whether cmd.Wait already returned during the grace period, so the caller
// never reaps twice and SIGKILL never reaches a pid recycled by the OS.
func (c CLI) kill(cmd *exec.Cmd, done <-chan error) bool {
	pgid := -cmd.Process.Pid

	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil && c.Logger != nil {
		c.Logger.Warn("sigterm to engine group failed", "engine", c.EngineName, "error", err)
	}

	grace := time.NewTimer(2 * time.Second)
	defer grace.Stop()

	select {
	case <-done:
		return true
	case <-grace.C:
	}

	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil &&
		!errors.Is(err, syscall.ESRCH) && c.Logger != nil {
		c.Logger.Warn("sigkill to engine group failed", "engine", c.EngineName, "error", err)
	}

	return false
}

// limitedWriter discards everything past remaining bytes instead of failing,
// because a truncated log is better than an OOM-killed worker.
type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}

	chunk := p
	if int64(len(chunk)) > l.remaining {
		chunk = chunk[:l.remaining]
	}

	n, err := l.w.Write(chunk)
	l.remaining -= int64(n)

	if err != nil {
		return n, fmt.Errorf("buffering engine output: %w", err)
	}

	return len(p), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}

	if len(s) > 300 {
		s = s[:300]
	}

	return s
}
