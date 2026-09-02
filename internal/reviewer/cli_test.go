package reviewer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stub writes an executable shell script and returns its path, so the adapter
// is exercised against a real spawned process rather than a mocked exec.
func stub(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "engine.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("writing stub: %v", err)
	}

	return path
}

func newCLI(t *testing.T, script string, timeout time.Duration) CLI {
	t.Helper()

	return CLI{
		EngineName:     "stub",
		Binary:         stub(t, script),
		Timeout:        timeout,
		MaxFindings:    5,
		MaxOutputBytes: 1 << 20,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestCLIParsesFindingsAndSeesThePromptOnStdin(t *testing.T) {
	// The stub echoes back whether the diff reached it on stdin.
	script := `input=$(cat)
case "$input" in
  *"MARKER-DIFF"*) echo '{"summary":"ok","findings":[{"file":"a.go","line":2,"severity":"major","title":"t","body":"b"}]}' ;;
  *) echo '{"summary":"prompt did not arrive","findings":[]}' ;;
esac`

	res, err := newCLI(t, script, 10*time.Second).
		Review(context.Background(), Request{Repo: "r", PRNumber: 1, Diff: "MARKER-DIFF"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	if res.Summary != "ok" {
		t.Fatalf("Summary = %q; the prompt likely never reached stdin", res.Summary)
	}

	if len(res.Findings) != 1 || res.Findings[0].File != "a.go" {
		t.Fatalf("Findings = %+v", res.Findings)
	}
}

// A CLI authenticated by subscription login reads the OS keyring, and that
// lookup is keyed by the account name. Stripping USER/LOGNAME made the engine
// fail with "Invalid API key" on a host where the CLI worked interactively.
func TestCLIChildEnvCarriesKeyringIdentityButNoSecrets(t *testing.T) {
	t.Setenv("USER", "prw")
	t.Setenv("LOGNAME", "prw")
	t.Setenv("PRW_GITHUB_TOKEN", "ghp-must-not-leak")

	// The stub reports its own environment, so this asserts what the engine
	// actually receives rather than what the allowlist says.
	res, err := newCLI(t, `cat >/dev/null
echo "{\"summary\":\"$(env | sort | tr '\n' ';')\",\"findings\":[]}"`, 10*time.Second).
		Review(context.Background(), Request{Repo: "r", PRNumber: 1, Diff: "d"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	// The environment is only ever asserted against, never echoed: printing it
	// on failure would copy whatever credentials it carries into the test log.
	for _, want := range []string{"USER=prw", "LOGNAME=prw"} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("engine env is missing %s; keyring auth would fail", want)
		}
	}

	if strings.Contains(res.Summary, "ghp-must-not-leak") {
		t.Error("the forge token reached the engine environment")
	}
}

func TestCLIModelOverridesInheritedAnthropicModel(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "claude-3-7-sonnet-20250219")

	engine := newCLI(t, `cat >/dev/null
echo "{\"summary\":\"$(env | sort | tr '\n' ';')\",\"findings\":[]}"`, 10*time.Second)
	engine.Model = "claude-sonnet-5"

	res, err := engine.Review(context.Background(), Request{Repo: "r", PRNumber: 1, Diff: "d"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	if !strings.Contains(res.Summary, "ANTHROPIC_MODEL=claude-sonnet-5") {
		t.Error("Model did not override the inherited ANTHROPIC_MODEL")
	}

	if strings.Contains(res.Summary, "claude-3-7-sonnet-20250219") {
		t.Error("the retired model id from the shell leaked through instead of being overridden")
	}
}

func TestCLIDetectsRateLimitSignals(t *testing.T) {
	cases := []struct {
		name   string
		script string
	}{
		{"claude usage limit", `echo "Claude usage limit reached. Your limit will reset at 3pm" >&2; exit 1`},
		{"http 429", `echo "API error: 429 Too Many Requests" >&2; exit 1`},
		{"overloaded 529", `echo '{"type":"overloaded_error"}' >&2; exit 1`},
		{"quota exceeded", `echo "quota exceeded for this account" >&2; exit 2`},
		{"retry after", `echo "rate-limited, retry-after: 60" >&2; exit 1`},
		{"zero exit with notice", `echo "Claude usage limit reached"; exit 0`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := newCLI(t, c.script, 10*time.Second).
				Review(context.Background(), Request{Diff: "d"})
			if !errors.Is(err, ErrRateLimited) {
				t.Fatalf("err = %v, want ErrRateLimited", err)
			}
		})
	}
}

func TestCLIOtherFailureIsNotRateLimited(t *testing.T) {
	_, err := newCLI(t, `echo "syntax error in prompt" >&2; exit 3`, 10*time.Second).
		Review(context.Background(), Request{Diff: "d"})

	if err == nil {
		t.Fatal("want error")
	}

	if errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, must not be classified as rate limiting", err)
	}
}

func TestCLIRateLimitTextWithValidFindingsIsNotAFallback(t *testing.T) {
	// A review that legitimately discusses rate limiting must not be mistaken
	// for the engine itself being rate limited.
	script := `echo '{"summary":"the rate limit code is wrong","findings":` +
		`[{"file":"a.go","line":1,"severity":"major","title":"429 not handled","body":"b"}]}'`

	res, err := newCLI(t, script, 10*time.Second).Review(context.Background(), Request{Diff: "d"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("Findings = %+v", res.Findings)
	}
}

func TestCLITimeoutKillsTheProcessGroup(t *testing.T) {
	// The stub ignores SIGTERM, which is exactly the case a plain cmd.Wait
	// with a context would hang on.
	script := `trap '' TERM
sleep 30`

	start := time.Now()

	_, err := newCLI(t, script, 300*time.Millisecond).Review(context.Background(), Request{Diff: "d"})
	if err == nil {
		t.Fatal("want timeout error")
	}

	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}

	// SIGTERM grace period is 2s; anything near 30s means the kill failed.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("took %s: the hung engine was not killed", elapsed)
	}
}

// A shutdown and an overrunning engine both cancel the invocation, but they are
// different incidents: reporting the first as a timeout sends someone hunting a
// slow engine that was behaving.
func TestCLIParentCancellationIsNotReportedAsATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	// The timeout is long enough that only the parent's cancellation can end
	// this invocation.
	_, err := newCLI(t, "sleep 30", time.Hour).Review(ctx, Request{Diff: "d"})
	if err == nil {
		t.Fatal("want an error")
	}

	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v, want it reported as a cancellation", err)
	}

	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, must not blame a timeout for the parent's cancellation", err)
	}
}

func TestCLIMissingBinary(t *testing.T) {
	cli := CLI{
		EngineName:     "absent",
		Binary:         "/nonexistent/engine-binary",
		Timeout:        time.Second,
		MaxOutputBytes: 1024,
	}

	if _, err := cli.Review(context.Background(), Request{Diff: "d"}); err == nil {
		t.Fatal("want error for missing binary")
	}
}

func TestCLIUnparsableOutput(t *testing.T) {
	_, err := newCLI(t, `echo "I would rather chat about this"`, 10*time.Second).
		Review(context.Background(), Request{Diff: "d"})

	if err == nil || !strings.Contains(err.Error(), "output") {
		t.Fatalf("err = %v, want an output parse failure", err)
	}
}

func TestCLIOutputIsCappedWithoutFailing(t *testing.T) {
	cli := newCLI(t, `echo '{"summary":"ok","findings":[]}'`, 10*time.Second)
	cli.MaxOutputBytes = 8 // truncates mid-JSON

	if _, err := cli.Review(context.Background(), Request{Diff: "d"}); err == nil {
		t.Fatal("want a parse error from truncated output, not a crash")
	}
}

func TestLimitedWriterReportsFullLengthWritten(t *testing.T) {
	var sink strings.Builder

	w := &limitedWriter{w: &sink, remaining: 4}

	n, err := w.Write([]byte("0123456789"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if n != 10 {
		t.Fatalf("n = %d, want 10 so the writer never reports a short write", n)
	}

	if sink.String() != "0123" {
		t.Fatalf("buffered %q, want %q", sink.String(), "0123")
	}

	if _, err := w.Write([]byte("more")); err != nil {
		t.Fatalf("Write past limit: %v", err)
	}

	if sink.String() != "0123" {
		t.Fatalf("buffered %q after the cap", sink.String())
	}
}

func TestFirstLineTruncates(t *testing.T) {
	if got := firstLine("  one\ntwo\n"); got != "one" {
		t.Fatalf("firstLine = %q", got)
	}

	if got := firstLine(strings.Repeat("x", 500)); len(got) != 300 {
		t.Fatalf("len = %d, want 300", len(got))
	}
}

func verifyRequest() VerifyRequest {
	return VerifyRequest{
		Repo:     "o/r",
		PRNumber: 7,
		Title:    "Add trace id",
		Diff:     "@@ delta @@",
		Threads:  []OpenThread{{ID: "T1", File: "a.ts", Line: 30, Finding: "bug", Replies: []string{"dev: fixed"}}},
	}
}

func TestCLIVerifyParsesVerdicts(t *testing.T) {
	script := `cat >/dev/null
echo '{"verdicts":[{"id":"T1","verdict":"fixed","note":"looks right"}]}'`

	res, err := newCLI(t, script, 10*time.Second).Verify(context.Background(), verifyRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(res.Verdicts) != 1 || res.Verdicts[0].Verdict != VerdictFixed {
		t.Fatalf("verdicts = %+v", res.Verdicts)
	}
}

// The prompt goes on stdin for Verify exactly as it does for Review, so a large
// diff never hits ARG_MAX.
func TestCLIVerifySeesThePromptOnStdin(t *testing.T) {
	script := `input=$(cat)
case "$input" in
  *"thread id: T1"*) echo '{"verdicts":[{"id":"T1","verdict":"fixed","note":"n"}]}' ;;
  *) echo '{"verdicts":[]}' ;;
esac`

	res, err := newCLI(t, script, 10*time.Second).Verify(context.Background(), verifyRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(res.Verdicts) != 1 {
		t.Fatal("the verify prompt did not reach the engine on stdin")
	}
}

func TestCLIVerifyWithNoThreadsDoesNotRunTheEngine(t *testing.T) {
	// A stub that would fail loudly if it ever ran.
	res, err := newCLI(t, "exit 1", 10*time.Second).Verify(context.Background(), VerifyRequest{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(res.Verdicts) != 0 {
		t.Fatalf("verdicts = %+v, want none", res.Verdicts)
	}
}

func TestCLIVerifyDetectsRateLimiting(t *testing.T) {
	script := `cat >/dev/null
echo "Claude usage limit reached" >&2
exit 1`

	_, err := newCLI(t, script, 10*time.Second).Verify(context.Background(), verifyRequest())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited so the chain falls back", err)
	}
}

func TestCLIVerifyRateLimitTextWithVerdictsIsNotAFallback(t *testing.T) {
	script := `cat >/dev/null
echo '{"verdicts":[{"id":"T1","verdict":"unfixed","note":"the rate limit handling is still wrong"}]}'`

	res, err := newCLI(t, script, 10*time.Second).Verify(context.Background(), verifyRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if len(res.Verdicts) != 1 {
		t.Fatal("a verdict discussing rate limiting was misread as the engine being rate limited")
	}
}

func TestCLIVerifyUnparsableOutput(t *testing.T) {
	_, err := newCLI(t, "cat >/dev/null\necho not json", 10*time.Second).
		Verify(context.Background(), verifyRequest())
	if err == nil {
		t.Fatal("err = nil, want a failure on output carrying no JSON")
	}
}

func TestCLIVerifyFailureCarriesTheEngineOutput(t *testing.T) {
	script := `cat >/dev/null
echo "engine exploded" >&2
exit 3`

	_, err := newCLI(t, script, 10*time.Second).Verify(context.Background(), verifyRequest())
	if err == nil || !strings.Contains(err.Error(), "engine exploded") {
		t.Fatalf("err = %v, want the engine's message for diagnosis", err)
	}
}
