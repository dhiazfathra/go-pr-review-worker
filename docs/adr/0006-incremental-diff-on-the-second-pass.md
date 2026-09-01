# ADR-0006: Scope the second pass to the diff since the last reviewed SHA

## Status

Accepted (2026-09-01)

## Context

The second pass runs after the author pushes fixes. Handing the engine the full
PR diff again would re-derive the findings already posted (dedup would then
discard most of its output), spend quota proportional to PR size rather than to
what changed, and bury the new code in unchanged context.

## Decision

- Cycle 1: the full PR diff (`GET .../pulls/{n}` with the diff media type;
  `.../merge_requests/{iid}/changes` on GitLab).
- Cycle 2: `compare(last_reviewed_sha, head_sha)` — GitHub's three-dot compare,
  GitLab's `repository/compare`.
- If the comparison fails (a force-push can orphan the previously reviewed SHA,
  making it unresolvable), fall back to the full diff and log the fallback.
- If the resulting diff is blank, skip the pass and do not spend a cycle.

The prompt tells the engine which pass it is on, so pass 2 is explicitly framed
as "only what changed since the previous review".

## Alternatives considered

### Full diff on both passes

- Pros: simplest; the engine always sees complete context.
- Cons: cost scales with PR size on every push, and the engine tends to
  re-report unchanged code, relying entirely on dedup to stay quiet.
- Rejected on cost and signal, though the fallback path keeps it available.

### Local clone plus `git diff`

- Pros: full file context, not just hunks.
- Cons: a working copy, credentials for clone, disk on a small VM, and cleanup
  after every job.
- Rejected: the provider APIs already return exactly the diff needed.

## Consequences

- Pass 2 is fast and cheap on large PRs.
- The engine cannot see unchanged code it did not receive, so a pass-2 finding
  about pre-existing code will not appear. Acceptable: pass 1 covered it.
- The fallback keeps force-pushes working instead of failing the job.
