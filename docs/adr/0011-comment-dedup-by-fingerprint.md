# ADR-0011: Deduplicate comments by a file+title fingerprint, and post one summary per pass

## Status

Accepted (2026-09-01)

## Context

The second pass may re-report an issue the author has not addressed yet, and a
retried job may re-run a pass that already posted comments. Posting the same
comment twice makes the reviewer look broken and buries the new findings.
Wording is not stable across runs — the same defect comes back reworded — so
comparing bodies would not catch a repeat.

## Decision

- **Inline comments**: fingerprint = `sha256(lower(file + "\x00" + title))[:8]`.
  NUL, not `|`, joins the two fields, so the join is injective — `("a|b", "c")`
  and `("a", "b|c")` would otherwise fingerprint identically under a `|` join.
  Injectivity holds only because the engine's output is not trusted to be
  NUL-free: JSON can encode `\u0000`, so `parseResult` drops any finding whose
  file or title contains one rather than letting it collide with another
  finding and silently suppress it. The body and the line number are excluded:
  a reworded body or a shifted line is the same finding.
  `posted_comments(pr_key, fingerprint)` is a primary key, so
  `Store.ClaimFingerprints` atomically returns only the ones never posted for
  that PR and records them in the same step.
- **Summary comment**: a new comment per cycle, so pass 1's summary stays
  visible next to pass 2's. Within a cycle, a retry edits the existing summary
  in place (`pr_reviews.summary_comment_id` + `summary_cycle`), so a retried job
  never leaves two summaries for the same pass.
- Findings below `PRW_MIN_SEVERITY` are dropped, and each pass posts at most
  `PRW_MAX_COMMENTS`.

## Alternatives considered

### Fingerprint including the line number

- Rejected: any edit above the finding shifts the line and the same issue is
  posted again.

### Fingerprint the full body

- Rejected: wording is not reproducible across model runs, so this deduplicates
  almost nothing.

### Fetch existing comments from the API and compare text

- Pros: no local state; survives a lost database.
- Cons: an extra paginated API call per pass, and text comparison is exactly the
  unreliable part.
- Rejected; the local index is cheaper and deterministic.

### Update one summary comment in place across both passes

- Pros: a single tidy comment.
- Rejected: it destroys pass 1's record, and the diff between passes is what a
  reviewer actually wants to see.

## Consequences

- Comment posting is idempotent, which is what makes crash recovery safe
  ([ADR-0002](0002-sqlite-for-queue-and-review-budget.md)).
- A finding whose title the engine rewords between passes will be posted twice.
  Accepted: the prompt asks for stable titles, and the alternative (body
  matching) is worse.
- Deleting `posted_comments` rows re-enables posting for that PR — a deliberate
  manual override.
