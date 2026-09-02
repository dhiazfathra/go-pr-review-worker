# ADR-0012: Retry, then dead-letter with a note on the pull request

## Status

Accepted (2026-09-01)

## Context

A review can fail for transient reasons (a 502 from the forge, a timeout, both
engines rate-limited at once) or permanent ones (a malformed prompt, an
unconfigured provider). The worst outcome is a silent drop: the author sees no
comments and concludes the code is clean.

## Decision

- `jobs.attempts` increments on every claim. Below `PRW_MAX_ATTEMPTS`
  (default 3) a failure sets the job back to `failed`, which `ClaimNext` treats
  as runnable, after a fixed `PRW_RETRY_DELAY` (default 30s).
- At the limit the job becomes `dead` and the worker posts one comment on the PR:
  _"Automated review failed after N attempts"_, with the error text.
- A failed pass never advances `pr_reviews.cycle`, so the budget is not spent on
  a review that did not happen.
- Dead rows stay in the table as the dead-letter record; `last_error` holds the
  cause.

## Alternatives considered

### Exponential backoff

- Pros: gentler on a struggling upstream.
- Cons: with one worker and a fixed 3-attempt ceiling, the total wait is bounded
  anyway; the extra state buys little.
- Deferred, not rejected — `RetryDelay` is the single knob to change.

### Retry forever

- Rejected: an unfixable job would occupy the head of a FIFO queue and starve
  everything behind it.

### Drop silently and rely on logs

- Rejected: nobody reads the logs of a healthy-looking box. The PR is where the
  reader is.

### A separate `dead_letters` table

- Rejected: `jobs.state = 'dead'` plus `last_error` is the same information
  without a second table to join.

## Consequences

- Every failure is visible in two places: the PR and `jobs.last_error`.
- A transient forge outage self-heals within three attempts.
- Requeueing a dead job is a manual `UPDATE jobs SET state='queued'`, which is
  deliberate — an operator decides.
