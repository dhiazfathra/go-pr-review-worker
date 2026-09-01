# ADR-0004: Two review passes per pull request, then a single notice

## Status

Accepted (2026-09-01)

## Context

An automated reviewer that comments on every push becomes noise, and every pass
costs agent quota. The requirement is at most two passes per PR: one when it
opens, one after the follow-up push. Pushes after that must not be reviewed —
and the behaviour on those pushes needs to be decided, not left implicit.

## Decision

- `pr_reviews.cycle` counts *completed* passes, keyed by
  `provider:repo#number` — not by webhook delivery, not by branch name.
- Cycle 1 is triggered by `opened` / `reopened` / `ready_for_review` (GitHub) or
  `open` / `reopen` (GitLab); cycle 2 by `synchronize` (GitHub) or `update` with
  an `oldrev` (GitLab).
- On the first push after the budget is spent, the worker posts **one** comment
  saying the budget is exhausted, records `budget_notice_posted`, and is silent
  for every later push.
- A pass that fails, or that finds an empty diff, does **not** consume a cycle.

Suppress the notice entirely with `PRW_ANNOUNCE_BUDGET_EXHAUSTED=false`.

## Alternatives considered

### Stay completely silent after the budget

- Pros: zero noise.
- Rejected as the default: silence is indistinguishable from "the reviewer found
  nothing" or "the reviewer is broken". The author waits for a review that will
  never arrive.

### Repeat the notice on every push

- Rejected: that is the noise the budget exists to prevent.

### Count cycles per head SHA rather than per PR

- Rejected: it makes the budget unbounded — every push would get its own budget,
  which is the opposite of the requirement.

## Consequences

- Reopening a PR does not reset the budget (the row is keyed by PR number and
  survives). Resetting it is a deliberate manual act: delete the row.
- The budget is persisted, so a restart mid-review cannot grant a third pass
  ([ADR-0002](0002-sqlite-for-queue-and-review-budget.md)).
- `PRW_MAX_CYCLES` exists for other deployments, but 2 is the specified default.
