# ADR-0009: Bound every engine invocation and kill its whole process group

## Status

Accepted (2026-09-01)

## Context

There is one worker. A hung CLI blocks every queued PR indefinitely — the single
worst failure mode in this design. Agentic CLIs are not simple filters either:
they spawn children (shells, language servers, MCP servers), so killing the
direct child can leave orphans holding memory on a 4 GB box.

`exec.CommandContext` alone is insufficient: it sends only `SIGKILL` to the
direct child by default and does nothing about the process group.

## Decision

Per invocation:

- `context.WithTimeout(PRW_ENGINE_TIMEOUT)`, default 10 minutes.
- `SysProcAttr{Setpgid: true}` puts the child in its own process group.
- On timeout: `SIGTERM` to `-pgid`, a 2 second grace period, then `SIGKILL` to
  `-pgid`; then reap `cmd.Wait` so no zombie remains.
- stdout and stderr are captured through a `limitedWriter` capped at 4 MB that
  *discards* the excess and reports a full write, so a runaway CLI truncates its
  log instead of exhausting RAM.

A timeout is an ordinary job failure: it retries, then dead-letters
([ADR-0012](0012-failure-handling-retry-dead-letter-note.md)).

## Alternatives considered

### `exec.CommandContext` with the default cancel behaviour

- Rejected: kills the child, not the group. Grandchildren survive.

### `SIGKILL` immediately, no grace period

- Rejected: a CLI in the middle of writing its JSON answer would lose it. Two
  seconds is cheap insurance for a run that may have taken minutes.

### A watchdog that restarts the whole worker

- Rejected: a heavier hammer that also loses the in-flight job's context, when
  the problem is scoped to one child process.

## Consequences

- A hung engine costs one timeout window, not the queue.
- The test suite proves this against a stub that traps `SIGTERM` — the exact
  case a naive implementation hangs on.
- The policy is Unix-specific (`Setpgid`, `syscall.Kill`). The target is Linux;
  a Windows port would need its own kill strategy.
