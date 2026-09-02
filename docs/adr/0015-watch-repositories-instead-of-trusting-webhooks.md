# ADR-0015: Poll watched repositories instead of trusting webhooks to be complete

## Status

Accepted (2026-09-02)

## Decision date context

On 2026-09-02 a pull request received review pass 1 and then never received
pass 2, despite a push that should have triggered it. Nothing was broken: the
worker had no webhook pointing at it for that repository, and the process was
not running when the push landed. Both are ordinary operational states, and
neither produces a signal — the pull request simply sits with a half-finished
review and no one is told.

## Context

Until now the only way a job entered the queue was
`POST /webhook/{github,gitlab}` ([ADR-0010](0010-verify-webhook-signatures-before-enqueue.md)).
That makes a webhook delivery a single point of failure for the entire product:

- A delivery that arrives while the worker is down is **gone**. GitHub retries
  a failed delivery, but not indefinitely, and a connection refused during a
  deploy window is a normal occurrence.
- A repository can have **no hook at all** — nothing in the worker notices, and
  a pull request there is never reviewed.
- A hook can be **removed or its secret rotated** without the worker knowing;
  every delivery then fails signature verification and is dropped by design.

In each case the worker's own state (`pr_reviews.last_reviewed_sha`) already
records exactly what it has reviewed, and the forge can be asked what the
current head is. The information needed to notice the gap was always available;
nothing was asking.

## Decision

Add a watcher (`internal/worker/watcher.go`) that polls the repositories named
in `PRW_WATCH_REPOS` every `PRW_WATCH_INTERVAL` (default 2m). For each open
pull request it compares the head SHA against `pr_reviews.last_reviewed_sha`
and enqueues a job when they differ.

Three properties make it safe to run alongside webhooks rather than instead of
them:

1. **The delivery id is identical to the webhook path's**
   (`provider:repo#number:head-sha`, [ADR-0005](0005-head-sha-idempotency-key.md)).
   A push seen by both mechanisms collapses into one row via
   `INSERT OR IGNORE`, so nothing is reviewed twice.
2. **A head already reviewed is skipped before it reaches the queue**, so a
   quiet repository costs one list call per interval and no queue churn.
3. **A pull request the worker has never seen is enqueued as `opened`**, not
   `synchronize`, so its first pass reviews the whole diff rather than an
   incremental one against a SHA that was never reviewed.

Webhooks stay the fast path — they react in seconds, the watcher in minutes —
and the watcher is the safety net. Watching nothing (the default) leaves
behaviour exactly as it was.

Drafts are skipped: a draft has not been offered for review, and spending the
two-pass budget ([ADR-0004](0004-two-review-cycles-per-pull-request.md)) before
the author asks for an opinion wastes it.

## Alternatives considered

### Rely on GitHub's webhook redelivery

- Pros: no new code, no polling cost.
- Cons: covers only the "worker briefly down" case, and only within the
  retry window. It does nothing for a repository with no hook, a rotated
  secret, or a longer outage. Rejected as a partial fix for one of three
  failure modes.

### A manual "review this PR now" command or endpoint

- Pros: simplest possible thing; would have replaced the hand-written SQL
  `INSERT` used to drive the worker manually during the 2026-09-02 incident.
- Cons: requires a human to notice the gap first, which is exactly what does
  not happen — an unreviewed PR looks identical to a PR whose review found
  nothing. Rejected as the primary mechanism; the watcher subsumes it.

### Poll every repository the token can see

- Pros: no configuration; nothing can be forgotten.
- Cons: on an organisation token that is hundreds of repositories per tick,
  and it silently opts every project into automated review. Rejected: the
  explicit `PRW_WATCH_REPOS` list keeps the blast radius a deliberate choice.

### Drop webhooks and poll only

- Pros: one code path instead of two.
- Cons: turns a seconds-latency review into a minutes-latency one for the
  common case, for no gain — the two mechanisms already deduplicate. Rejected.

## Consequences

- A push is reviewed even when it was never delivered, which is the point.
- The worker now makes API calls when nothing is happening: one
  `GET /pulls?state=open` per watched repository per interval. At the default
  2m that is 30 calls/hour/repository against GitHub's 5000/hour budget.
- A repository is only watched if someone lists it. A forgotten repository is
  still not reviewed — the watcher narrows the failure to configuration, it
  does not remove it.
- Startup now fails when `PRW_WATCH_REPOS` names a forge whose credentials are
  missing, rather than logging a failed poll every interval forever.
- A repository with more open pull requests than the paging cap allows makes
  the sweep **fail loudly** (`provider.ErrTooManyResults`) instead of
  returning a truncated list. A truncated list looks exactly like a complete
  one, so everything past the cap would go unreviewed with no signal — the
  same silent gap this ADR exists to close.
