# ADR-0002: Keep the queue and the review budget in one SQLite file

## Status

Accepted (2026-09-01)

## Context

Two pieces of state must outlive the process:

1. The job queue — a webhook accepted with `202` must eventually be reviewed,
   even if the worker restarts a second later.
2. The per-PR review cycle count — the hard limit of two review passes is
   meaningless if a restart resets the counter, because the same PR could then
   be reviewed a third time.

The earlier sketch tracked the cycle count in an in-memory map, which fails
exactly this test.

## Decision

One SQLite database (`PRW_DB`, default `prw.db`) holding three tables: `jobs`,
`pr_reviews`, `posted_comments`. `internal/store` is the only package that
touches it. The in-process channel is kept purely as a wake-up hint with buffer
1 — losing a hint costs at most one poll interval, never a job.

Connection settings: `journal_mode=WAL`, `busy_timeout=5000`,
`SetMaxOpenConns(1)`.

## Alternatives considered

### Redis or a real broker (NATS, RabbitMQ)

- Pros: purpose-built queues, visibility timeouts for free.
- Cons: a second process to install, monitor and keep alive, with its own RAM
  budget, to serialize a queue that by requirement has exactly one consumer.
- Rejected: the requirement is _one worker, one job at a time_. There is no
  distribution problem to solve.

### PostgreSQL

- Rejected: same objection, plus a server process, on a 4 GB box.

### A buffered Go channel alone

- Rejected: it is not durable. A restart drops queued jobs and, worse, drops
  the cycle count.

### A JSON file rewritten on each change

- Rejected: no atomic read-modify-write, and it would need hand-rolled locking
  to be as safe as a single SQLite transaction already is.

## Consequences

- `SetMaxOpenConns(1)` matches the single writer; `SQLITE_BUSY` storms cannot
  happen by construction.
- Backup is `sqlite3 prw.db "VACUUM INTO 'backup.db'"`, not `cp prw.db*`: in WAL
  mode a filesystem copy can catch the main file, `-wal`, and `-shm` in
  inconsistent states and lose committed-but-not-checkpointed transactions.
  `VACUUM INTO` (or the Online Backup API) produces a consistent snapshot
  without stopping the worker. Inspection is `sqlite3 prw.db`.
- The queue is polled rather than pushed, so a lost notification degrades
  latency, not correctness.
- A job left in `running` by a crash is picked up again on start. That is safe
  only because posting is idempotent by fingerprint
  ([ADR-0011](0011-comment-dedup-by-fingerprint.md)) — the two decisions must
  stay together.
