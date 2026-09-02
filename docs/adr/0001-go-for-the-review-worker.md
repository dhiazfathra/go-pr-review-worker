# ADR-0001: Write the review worker in Go

## Status

Accepted (2026-09-01)

## Context

The worker is a long-lived daemon on a resource-constrained VM (target: 2 vCPU /
4 GB, shared with nothing else). It must: serve two webhook endpoints, keep a
durable FIFO queue, spawn an agentic CLI and stream its stdout/stderr, and post
to the GitHub and GitLab REST APIs. Idle memory matters more than throughput —
the process is idle most of the day and does exactly one job at a time when it
is not.

There is also prior art: an existing Go CodeRabbit-style reviewer for GitLab
merge requests, whose architecture (webhook intake, job, provider client,
prompt contract) maps onto this problem almost unchanged.

## Decision

Go, one static binary, `cmd/pr-review-worker`.

## Alternatives considered

### Rust

- Pros: lowest idle RSS, no GC, `tokio::process` streams child output well,
  `octocrab` exists for GitHub.
- Cons: GitLab API coverage is thinner; the existing reviewer would be a
  rewrite rather than a port; compile times slow the review-fix loop this
  project itself depends on.
- Rejected: the memory advantage is real but small at this scale. The worker's
  resident set is dominated by the _child_ CLI (hundreds of MB for Node-based
  agentic CLIs), not by the supervisor. Trading a working architecture for
  ~10 MB of RSS is a bad exchange.

### Node.js / TypeScript

- Pros: same runtime as the Claude Code CLI, richest webhook ecosystem
  (`@octokit/webhooks`).
- Cons: ~40-60 MB idle baseline before any work, no single-binary deploy,
  `node_modules` on the VM.
- Rejected: it doubles the interpreter footprint on a box that already pays for
  one when the CLI runs.

### Python

- Rejected for the same footprint and packaging reasons, with weaker
  process-supervision ergonomics than either alternative.

## Consequences

- Deployment is `scp` one binary plus a systemd unit; no runtime to install.
- `os/exec` with `SysProcAttr.Setpgid` gives exactly the child-process control
  the timeout policy needs (see [ADR-0009](0009-engine-timeout-and-process-group-kill.md)).
- GC pauses are irrelevant: the hot path is one subprocess per several minutes.
- The pure-Go SQLite driver (`modernc.org/sqlite`) keeps the binary cgo-free at
  the cost of ~16 MB of binary size. Acceptable; disk is not the constraint.
