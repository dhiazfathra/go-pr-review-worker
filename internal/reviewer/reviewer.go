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

// Finding is one inline review comment anchored to a line of the new file.
type Finding struct {
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
}

// Fingerprint identifies a finding for dedup across review cycles. The body is
// deliberately excluded: the same issue reworded must not be posted twice.
func (f Finding) Fingerprint() string {
	sum := sha256.Sum256([]byte(strings.ToLower(f.File + "|" + f.Title)))

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
		if f.File == "" || f.Title == "" {
			continue // unanchorable, would post as a comment on nothing
		}

		if _, ok := severityRank[f.Severity]; !ok {
			f.Severity = SeverityMinor
		}

		kept = append(kept, f)
	}

	res.Findings = kept

	return res, nil
}

// extractJSON returns the outermost JSON object in s, scanning from the last
// opening brace that yields balanced output. String literals are respected so
// a brace inside a comment body does not confuse the scan.
func extractJSON(s string) (string, error) {
	start := strings.Index(s, "{")
	if start < 0 {
		return "", fmt.Errorf("no json object in engine output (%d bytes)", len(s))
	}

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
				return s[start : i+1], nil
			}
		}
	}

	return "", errors.New("unbalanced json object in engine output")
}
