# ADR-0003: One worker goroutine, one global FIFO, no concurrency

## Status

Accepted (2026-09-01)

## Context

Each review spawns an agentic coding CLI, which is the expensive part: hundreds
of megabytes of resident memory and a CPU-hungry startup. Two concurrent
reviews on a 2 vCPU / 4 GB VM would either swap or be OOM-killed. Provider API
rate limits and the agent vendor's own usage limits also serialize better than
they parallelize.

## Decision

Exactly one `Worker.Run` goroutine. It claims one job, runs it to completion,
and only then claims the next. Ordering is global FIFO by `jobs.id` — jobs from
different repositories and PRs interleave in arrival order, with no per-repo
fairness scheduling.

`Store.ClaimNext` performs the claim inside a transaction, so even a
hypothetical second worker could not take the same job twice.

## Alternatives considered

### A worker pool sized by CPU count

- Rejected: violates the stated constraint and the memory budget. The bottleneck
  is the child CLI, not the supervisor.

### Per-repository queues with round-robin

- Pros: one busy monorepo cannot starve other repositories.
- Cons: needs a scheduler, and the starvation it prevents is hypothetical at
  the current volume (a handful of PRs per day).
- Deferred: `jobs` already carries `repo`, so a scheduler can be added later
  without a schema change.

## Consequences

- Queue latency during a burst is the sum of the reviews ahead of it. This is
  visible and acceptable; `/healthz` reports `pending_jobs` so the depth is
  observable.
- No lock contention, no cross-review interference, no partial-review races.
- One hung CLI would block everything, which is why the timeout policy in
  [ADR-0009](0009-engine-timeout-and-process-group-kill.md) is mandatory rather
  than a nicety.
