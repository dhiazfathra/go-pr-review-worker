# ADR-0005: Key webhook idempotency on the head SHA, not the delivery id

## Status

Accepted (2026-09-01)

## Context

Both forges redeliver webhooks: GitHub retries failed deliveries with the
*same* `X-GitHub-Delivery` UUID and offers manual redelivery from the UI;
GitLab retries on timeout. A redelivery must not consume a review cycle or
duplicate comments. Separately, two distinct deliveries can describe the same
state (a `synchronize` fired twice for one push, or `reopened` after `opened`
with no new commits).

## Decision

The unique key stored in `jobs.delivery_id` is derived from the payload, not
from the transport:

```
<provider>:<repo>#<number>:<head sha>
```

`INSERT OR IGNORE` makes a second delivery for the same head a no-op; the
handler answers `200` instead of `202` and enqueues nothing. The worker adds a
second guard: a job whose head SHA equals `pr_reviews.last_reviewed_sha` is
skipped without spending a cycle.

## Alternatives considered

### Use the provider delivery UUID

- Pros: literally the transport's idempotency key.
- Cons: covers only the retry case. Two independent deliveries describing the
  same commit both pass, and each would spend a cycle.
- Rejected: it protects against the narrower failure.

### Deduplicate at posting time only

- Rejected: comments would be deduped, but the cycle counter and the agent
  quota would still be spent on a redundant review.

## Consequences

- Redelivery is free and silent; the log records `duplicate delivery ignored`.
- A force-push that resets the branch to a previously reviewed SHA is treated as
  already reviewed. Correct: the tree under review is identical.
- Because the key is content-derived, `X-GitHub-Delivery` is not required to be
  present or unique.
