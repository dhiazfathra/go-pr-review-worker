# ADR-0016: Judge the diff, not the reply, before resolving a thread

## Status

Accepted (2026-09-02)

## Context

Until now the worker was write-only in a conversation it started: it posted
findings and never read what happened next. In practice a review thread has a
second half — the author pushes a fix and replies "Fixed in `783950b`" — and
nobody closes the loop. The threads stay open, the pull request accumulates
stale unresolved conversations, and a human has to re-read each one to decide
whether the worker's objection still stands.

Automating that half is not the same problem as reviewing a diff. The failure
mode is asymmetric:

- **Resolving a thread that is not actually fixed** removes the only visible
  trace of a real defect. The finding is gone from the reviewer's list and the
  pull request looks clean.
- **Leaving a fixed thread open** costs the author one more click.

The first is much worse than the second, and the obvious cheap implementation —
trust a reply that says "fixed" — optimises for exactly the wrong one. A reply
is a claim by an interested party; the diff is the evidence.

## Decision

Add a follow-up pass (`internal/worker/verify.go`) that runs before the review
pass on every job:

1. List the pull request's review threads, keep the **unresolved** ones whose
   first comment carries the worker's own marker. A human's thread is not the
   worker's to close.
2. Send each one to the engine with its original finding, the human replies,
   and **the diff since the last reviewed commit**. The prompt says in as many
   words: _judge the diff, not the reply; a reply saying "fixed" with no
   matching code change is NOT fixed._
3. Act on the verdict:
   - `fixed` → resolve the thread, leave a short confirming reply.
   - `partial` / `unfixed` → **leave it open** and reply with the engine's
     reason, so the author gets a specific objection rather than silence.
   - `unrelated` → touch nothing. No evidence is not a verdict.

Four rules make the asymmetry explicit in the code rather than only in the
prompt:

- A verdict naming a thread that was not asked about is **dropped**
  (`parseVerifyResult`), so a hallucinated id can never resolve a real thread.
- An **unrecognised** verdict string is downgraded to `unrelated`, not guessed
  at — a typo must never read as `fixed`.
- A thread whose `resolveReviewThread` call **fails** stays counted as open, so
  the next pass retries it and an approval cannot slip through behind it.
- The worker's **own replies are filtered out** of what it reads back, or it
  would eventually treat its own earlier opinion as corroboration.

The pass does **not** consume a review cycle
([ADR-0004](0004-two-review-cycles-per-pull-request.md)). Answering the author
about findings already reported is not a new review, and charging the budget
for it would mean any pull request that gets a follow-up never receives its
second pass. It therefore also runs _after_ the budget is spent — at that
point it is the only thing the worker still owes the pull request.

### Approving

When every thread the worker opened is resolved and the same pass found nothing
new, it may submit an approving review. This is **off by default**
(`PRW_APPROVE_WHEN_RESOLVED`). An approval can satisfy a branch protection rule
and unblock a merge, which is a different class of act from leaving a comment:
it should be a deliberate decision by whoever runs the worker, not a
consequence of upgrading it. `pr_reviews.approved` makes it once-only.

Approval is withheld when the same pass posted a new finding — approving and
objecting in one breath is incoherent — and when any thread is still open.

A **refused** approval is logged and the pull request left unapproved; it does
not fail the job. The forge refuses for reasons that are permanent and none of
them mean the review went wrong: `422 Can not approve your own pull request`
when the token's account opened the pull request, `403` when it cannot review
the repository. Returning an error there retried a call that could never
succeed and then dead-lettered a pull request whose findings were posted and
whose threads were resolved — telling the author the review failed when it had
not. Found by
[evidence 0002](../evidence/0002-watcher-and-reply-verification/a-refused-approval-does-not-fail-the-job.md),
which is also why `approve` returns a plain `bool`.

### Provider capability

Resolving a thread on GitHub has no REST equivalent; it is a GraphQL mutation
(`resolveReviewThread`) and the thread's node id only comes back from GraphQL.
Rather than widen `Provider` with four methods most of which one forge cannot
implement, the capability is a second interface, `ThreadReviewer`, satisfied by
GitHub only. GitLab keeps working as before and the worker skips the follow-up
where it is unsupported — an interface a type cannot honour is better not
satisfied than satisfied with errors
([ADR-0013](0013-hand-rolled-rest-clients.md) keeps the same spirit for the
clients themselves).

## Alternatives considered

### Resolve whenever the author replies

- Pros: trivial; no engine call, no diff fetch.
- Cons: makes "type the word fixed" sufficient to silence a review. It is the
  cheapest implementation and the one that breaks the feature's only real
  guarantee. Rejected outright.

### Re-run the full review and resolve threads whose finding is gone

- Pros: no new prompt contract; reuses the review path.
- Cons: a finding's absence from a second review is weak evidence — the engine
  is non-deterministic, and the incremental diff deliberately excludes
  unchanged code, so almost every old finding would be "absent" and resolved.
  Rejected: it would resolve nearly everything for the wrong reason.

### Require a human to resolve, and only post the verdict as a reply

- Pros: never closes a real defect.
- Cons: leaves the stale-thread problem entirely unsolved, which is the reason
  for the feature. Rejected — but it is what `partial`/`unfixed`/`unrelated`
  do, so this behaviour is the fallback for every uncertain case.

### Approve by default when everything is resolved

- Pros: completes the loop with no configuration.
- Cons: an approval can satisfy branch protection and unblock a merge. Making
  that appear on upgrade, without anyone opting in, is a change in what the
  tool is allowed to do. Rejected in favour of an explicit flag.

## Consequences

- A pull request that fixes what the worker reported ends with its threads
  resolved instead of a list of stale conversations.
- An author who claims a fix without making one gets a specific, cited
  objection rather than a resolved thread.
- One extra engine invocation per job that has open worker threads, plus one
  GraphQL query. Both are skipped entirely when there is nothing open.
- The worker can now write to a pull request in three new ways (reply, resolve,
  approve), so its token needs `pull_requests: write`, and approval needs the
  account to be permitted to review the repository, and not to be the pull
  request's author. A token that cannot approve loses that call only: the
  comments, replies and resolves still stand and the job still succeeds.
- GitLab gets the watcher but not the follow-up pass until an equivalent
  discussions adapter exists.
