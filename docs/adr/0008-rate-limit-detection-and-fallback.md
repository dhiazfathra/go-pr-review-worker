# ADR-0008: Fall back to OpenCode only on a precisely matched rate-limit signal

## Status

Accepted (2026-09-01)

## Context

The primary engine is the Claude Code CLI. When its quota is exhausted the job
must automatically retry on OpenCode with no human in the loop. "Detect rate
limited" needs a precise definition, because falling back on _any_ failure would
burn the second engine's quota on failures that are not quota-related (a
malformed prompt, a missing binary, an unparsable diff) — those fail identically
on both engines.

## Decision

A failed invocation is classified as rate-limited when the combined
stdout+stderr matches, case-insensitively
(`internal/reviewer/cli.go:rateLimitSignals`):

- `usage limit reached`
- `limit reached ... resets`
- `rate limit` / `rate-limited` (any separator)
- `too many requests`
- `429`, `529`
- `overloaded_error`
- `quota exceeded` / `quota exhausted` / `insufficient quota`
- `retry-after`

Two refinements:

1. **Exit code 0 with a rate-limit notice** also counts, because a CLI may
   report quota trouble as an ordinary message; but only when the output
   contains no `"findings"` key — otherwise a review that legitimately discusses
   rate-limiting code would be misclassified.
2. Any other failure returns immediately without trying the fallback.

The chain (`reviewer.Chain`) advances only on `ErrRateLimited`, and the engine
that produced a result is named in the summary comment's footer.

## Alternatives considered

### Fall back on any non-zero exit

- Rejected: doubles the cost of every deterministic failure and hides the real
  error behind a second identical one.

### Exit-code-only classification

- Rejected: neither CLI reserves a distinct exit code for quota exhaustion, so
  the signal is not in the exit status.

### Parse a `Retry-After` header and wait instead of switching engines

- Pros: keeps quality on the primary engine.
- Cons: the single worker would sit idle for the entire window, blocking every
  queued PR — the opposite of what the fallback is for.
- Rejected; the fallback engine is the whole point.

## Consequences

- The signal list is a maintenance surface: vendors reword messages. It is
  covered by a table test per pattern, so a regression is a failing test rather
  than a silent stall.
- A false positive costs one wasted invocation on the fallback engine.
- A false negative surfaces as a dead-lettered job with the original message on
  the PR, which is diagnosable.
