// Package reviewer turns a diff into review findings by driving an agentic CLI
// in headless mode. The CLI is an implementation detail behind Engine, so the
// worker never names a binary and a third engine costs one adapter.
package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrRateLimited reports that an engine refused the request because of provider
// rate limiting or usage quota, and that another engine should be tried.
var ErrRateLimited = errors.New("engine rate limited")

// Severity ranks a finding. Anything below the configured minimum is dropped
// before posting, so the reviewer stays signal over noise.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
	SeverityNit      Severity = "nit"
)

var severityRank = map[Severity]int{
	SeverityNit:      0,
	SeverityMinor:    1,
	SeverityMajor:    2,
	SeverityCritical: 3,
}

// AtLeast reports whether s is at least as severe as min.
func (s Severity) AtLeast(min Severity) bool {
	return severityRank[s] >= severityRank[min]
}

// Rank returns s's numeric severity, highest for most severe, for sorting
// findings by severity.
func (s Severity) Rank() int {
	return severityRank[s]
}

// Finding is one inline review comment anchored to a line of the new file.
type Finding struct {
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
}

// Fingerprint identifies a finding for dedup across review cycles. The body is
// deliberately excluded: the same issue reworded must not be posted twice. The
// separator is NUL, not "|", because a file path or title could contain "|" —
// a "|"-joined ("a|b", "c") and ("a", "b|c") would otherwise fingerprint
// identically. The join is injective only because parseResult drops any
// finding whose file or title contains a NUL of its own.
func (f Finding) Fingerprint() string {
	sum := sha256.Sum256([]byte(strings.ToLower(f.File + "\x00" + f.Title)))

	return hex.EncodeToString(sum[:8])
}

// Result is the parsed output of one engine invocation.
type Result struct {
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
	// Engine names the adapter that produced the result, for the summary footer.
	Engine string `json:"-"`
}

// Request is everything an engine needs to review one diff.
type Request struct {
	Repo     string
	PRNumber int
	Title    string
	Body     string
	Diff     string
	// Cycle is 1 for the first review, 2 for the incremental one.
	Cycle int
	// PriorFindings are titles already posted, so the engine can avoid repeats.
	PriorFindings []string
}

// Engine reviews a diff. Implementations must be safe to call sequentially and
// must return ErrRateLimited (wrapped) when a fallback engine should take over.
type Engine interface {
	Name() string
	Review(ctx context.Context, req Request) (Result, error)
}

// Chain tries each engine in order, moving on only when an engine reports
// rate limiting. Any other failure is final: a broken diff or a bad prompt
// would fail identically on the fallback and only burn quota.
type Chain struct {
	Engines []Engine
}

// Review implements Engine.
func (c Chain) Review(ctx context.Context, req Request) (Result, error) {
	var errs []error

	for _, e := range c.Engines {
		res, err := e.Review(ctx, req)
		if err == nil {
			res.Engine = e.Name()

			return res, nil
		}

		errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))

		if !errors.Is(err, ErrRateLimited) {
			return Result{}, errors.Join(errs...)
		}
	}

	if len(errs) == 0 {
		return Result{}, errors.New("no engines configured")
	}

	return Result{}, errors.Join(errs...)
}

// Name implements Engine.
func (c Chain) Name() string {
	names := make([]string, 0, len(c.Engines))
	for _, e := range c.Engines {
		names = append(names, e.Name())
	}

	return strings.Join(names, "->")
}

// parseResult reads an engine's stdout. Agentic CLIs wrap JSON in prose or a
// fenced block however they like, so the last balanced JSON object wins.
func parseResult(out string) (Result, error) {
	raw, err := extractJSON(out)
	if err != nil {
		return Result{}, err
	}

	var res Result
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return Result{}, fmt.Errorf("decoding engine json: %w", err)
	}

	kept := make([]Finding, 0, len(res.Findings))

	for _, f := range res.Findings {
		if f.File == "" || f.Title == "" || f.Line <= 0 {
			continue // unanchorable, would post as a comment on nothing
		}

		// JSON can carry a literal NUL as a \u0000 escape, which would break the
		// injectivity the NUL-joined fingerprint relies on and let one finding
		// silently displace another. No real path or title contains one.
		if strings.ContainsRune(f.File, 0) || strings.ContainsRune(f.Title, 0) {
			continue
		}

		if _, ok := severityRank[f.Severity]; !ok {
			f.Severity = SeverityMinor
		}

		kept = append(kept, f)
	}

	res.Findings = kept

	return res, nil
}

// extractJSON returns the last JSON object in s that is both balanced and
// valid, preferring one that carries a "findings" or "summary" key over a
// status/telemetry object an agentic CLI may print first. String literals are
// respected so a brace inside a comment body does not confuse the scan.
func extractJSON(s string) (string, error) {
	if !strings.Contains(s, "{") {
		return "", fmt.Errorf("no json object in engine output (%d bytes)", len(s))
	}

	var (
		last             string
		lastFound        bool
		lastPayload      string
		lastPayloadFound bool
	)

	for start := 0; start < len(s); start++ {
		if s[start] != '{' {
			continue
		}

		candidate, ok := balancedObject(s, start)
		if !ok || !json.Valid([]byte(candidate)) {
			continue
		}

		last = candidate
		lastFound = true

		if strings.Contains(candidate, `"findings"`) || strings.Contains(candidate, `"summary"`) {
			lastPayload = candidate
			lastPayloadFound = true
		}

		// Advance past this object so a brace nested inside it is not also
		// tried as its own top-level candidate.
		start += len(candidate) - 1
	}

	if lastPayloadFound {
		return lastPayload, nil
	}

	if lastFound {
		return last, nil
	}

	return "", errors.New("unbalanced json object in engine output")
}

// balancedObject returns the substring of s starting at the brace at start and
// ending at its matching close brace, if there is one.
func balancedObject(s string, start int) (string, bool) {
	var (
		depth    int
		inString bool
		escaped  bool
	)

	for i := start; i < len(s); i++ {
		ch := s[i]

		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inString:
			escaped = true
		case ch == '"':
			inString = !inString
		case inString:
			// literal content, ignore braces
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}

	return "", false
}
