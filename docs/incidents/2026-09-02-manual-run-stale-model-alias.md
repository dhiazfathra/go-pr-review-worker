# Postmortem: Manual worker run against PR #111 posted a false "review failed" comment

**Date:** 2026-09-02 | **Duration:** ~7 minutes | **Severity:** SEV4
**Authors:** Dhiaz Fathra | **Status:** Resolved

### Summary

A manual, ad-hoc run of `pr-review-worker` against
[DermaestheticsGroup/dermaesthetics-new-backend#111](https://github.com/DermaestheticsGroup/dermaesthetics-new-backend/pull/111)
dead-lettered after 3 attempts and posted a "review failed" comment on the PR.
Root cause was a stale model alias resolving to a decommissioned Claude model
via the operator's local shell environment, not a defect in the worker. A
second run with the environment cleaned posted the real review successfully.

### Impact

- One incorrect "Automated review failed after 3 attempts" comment left on a
  real, external repo's PR (`dermaesthetics-new-backend#111`), still visible
  above the eventual correct review.
- No queue, data, or worker-state corruption. No user-facing service was
  running — this was a manual, throwaway invocation against a scratch SQLite
  DB, not the deployed worker.

### Timeline

| Time (UTC+7) | Event                                                                                                                                                                                                                                                                                                     |
| ------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 10:57        | First manual job enqueued for PR #111, head `69e4e7e...`. Attempt 1 failed: `API Error: 404 model: claude-3-7-sonnet-20250219`.                                                                                                                                                                           |
| 10:58        | Retried after unsetting `ANTHROPIC_BASE_URL`/`ANTHROPIC_CUSTOM_HEADERS` (proxy env inherited from the operator's own Claude Code session). Same 404 on attempt 3 — job dead-lettered, worker posted the failure comment to the PR per [ADR-0012](../adr/0012-failure-handling-retry-dead-letter-note.md). |
| 10:59        | Diagnosed: `claude` CLI has no `--model` flag in this version; global `~/.claude/settings.json` had `"model": "sonnet[1m]"`, which resolved to the retired dated model id.                                                                                                                                |
| 11:00        | Re-ran with `ANTHROPIC_MODEL=claude-sonnet-5` set explicitly. Job succeeded: 3 findings, posted as 1 major + 2 minor inline comments plus a summary.                                                                                                                                                      |

### Root Cause

The `claude` CLI picks its default model from ambient environment/config when
no explicit override is given. The operator's global Claude Code settings
carried a `"model": "sonnet[1m]"` alias that resolved to a since-retired
dated snapshot (`claude-3-7-sonnet-20250219`), which the Anthropic API now
rejects with `404 not_found_error`. The worker's engine chain (`reviewer.CLI`
→ `claude`, falling back to `opencode`) treated this as a normal engine
failure, retried per [ADR-0012](../adr/0012-failure-handling-retry-dead-letter-note.md),
exhausted its 3 attempts, and correctly followed policy by posting a
"failed, will not be retried" note — the worker behaved exactly as designed
given a broken engine invocation.

This was reachable specifically because the run was manual: `PRW_CLAUDE_ARGS`
was left at its default (no `--model`/no `ANTHROPIC_MODEL`), so the CLI fell
through to whatever model the operator's own interactive session had
configured, rather than a value the worker deployment pins.

### 5 Whys

1. Why did the review job dead-letter? → The `claude` CLI exited 1 on every attempt.
2. Why did `claude` exit 1? → The Anthropic API returned 404 for the model it requested.
3. Why did it request a nonexistent model? → It had no explicit model override, so it fell back to the ambient config/env.
4. Why was the ambient config pointing at a dead model? → The operator's global `~/.claude/settings.json` still had `"model": "sonnet[1m]"`, a stale alias from an earlier CLI/account setup, and `ANTHROPIC_BASE_URL` was also pointed at a local dev proxy from an unrelated session.
5. Why is a stale personal model alias able to affect a worker invocation at all? → The worker's engine config (`PRW_CLAUDE_ARGS`, `PRW_CLAUDE_BIN`) never pins a model explicitly, so it inherits ambient environment. That's a reasonable default for a systemd-deployed instance with its own clean environment, but it fails silently-into-wrong-model when invoked manually from an already-configured interactive shell.

### What Went Well

- [ADR-0012](../adr/0012-failure-handling-retry-dead-letter-note.md)'s
  retry-then-dead-letter-with-comment behavior worked exactly as designed:
  the failure was visible on the PR immediately, not silent.
- The two-cycle budget and idempotency-by-head-SHA meant re-running after the
  fix was safe — no duplicate jobs, no confusion about which SHA was reviewed.
- Diagnosis was fast: the 404's model id was specific enough to trace directly
  to a config value.

### What Went Poorly

- A stale, incorrect comment ("review failed... will not be retried") is now
  permanently on a real external PR, ahead of the correct review, until
  manually deleted or edited — cosmetic but confusing to anyone else reading
  the PR.
- No pre-flight check ensures the configured engine binary/model actually
  works before a job is claimed and burns a cycle attempt.
- Manual/ad-hoc worker invocations share environment with the operator's
  interactive `claude` session by default, which is a footgun distinct from
  the deployed (systemd) path.

### Action Items

| Action                                                                                                                                           | Owner        | Priority | Due Date    | Status                                                                                                                                                                                                    |
| ------------------------------------------------------------------------------------------------------------------------------------------------ | ------------ | -------- | ----------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Delete or edit the stale "Automated review failed" comment on `dermaesthetics-new-backend#111`                                                   | Dhiaz Fathra | P2       | 2026-09-03  | Done — comment deleted 2026-09-02                                                                                                                                                                         |
| Give the worker a way to pin the Claude model so it can never fall through to a stale account setting, and document it                           | Dhiaz Fathra | P2       | 2026-09-05  | Done — `PRW_CLAUDE_MODEL` added, forced into the subprocess env ahead of any inherited value; see [ADR-0014](../adr/0014-pin-anthropic-model-explicitly.md) and the README's "Engine environment" section |
| Consider a startup self-check (`claude --print` smoke test) before serving traffic, failing fast on a broken engine instead of only on first job | Dhiaz Fathra | P3       | unscheduled | Deferred — `PRW_CLAUDE_MODEL` removes the specific failure mode; a general engine-health probe is lower priority now                                                                                      |

### Lessons Learned

Ad-hoc/manual invocations of a worker that shells out to a CLI inherit the
operator's full environment unless explicitly scrubbed — including one-off
proxy/model overrides set up for an unrelated interactive coding session in
the same terminal. Treat that inherited environment as untrusted for any
manual test run, not just the deployed service's own env file.
