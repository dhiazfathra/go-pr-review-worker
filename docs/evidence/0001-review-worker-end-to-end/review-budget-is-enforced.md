# Evidence: a third push gets one notice, not a third review

Task: verify the hard constraint "max 2 review cycles per PR, persisted, keyed
by PR" against the live run on
[#1](https://github.com/dhiazfathra/go-pr-review-worker/pull/1) (commit
`80c6f79`), continuing from
[`two-review-passes-on-a-live-pr.md`](./two-review-passes-on-a-live-pr.md) where
`pr_reviews.cycle` had reached 2.

## Third push

`WEBHOOK_SECRET` held a throwaway value generated for this local run only. No
webhook was ever registered on the repository with it, so it granted nothing.

```bash
git push                       # 80c6f79
PRW_GITHUB_WEBHOOK_SECRET="$WEBHOOK_SECRET" \
  scripts/deliver.sh dhiazfathra/go-pr-review-worker 1 synchronize
```

```
HTTP 202
```

```json
{"level":"INFO","msg":"job enqueued","delivery":"github:dhiazfathra/go-pr-review-worker#1:80c6f79ed8c2b445c9b83c3c08f2b93c0c98d020","event":"synchronize","head":"80c6f79ed8c2b445c9b83c3c08f2b93c0c98d020"}
{"level":"INFO","msg":"job started","job":4,"attempt":1}
{"level":"INFO","msg":"job done","job":4,"duration":"1.348791416s"}
```

**1.3 seconds and no `engine finished` line.** The engine was never spawned: the
budget check happens before the diff is fetched, so a third push costs one
SQLite read and one comment, not a review.

What the author sees:

```bash
gh api repos/dhiazfathra/go-pr-review-worker/issues/1/comments \
  --jq '.[] | select(.body|startswith("<!-- pr-review-worker -->")) | .body' | grep -A1 budget
```

```
**Review budget exhausted** — this pull request already had its 2 automated
review passes. Further pushes will not be reviewed. Re-open the pull request to
reset the budget.
```

Total comments from the worker on the PR — two review summaries plus this one
notice, and nothing more:

```bash
gh api repos/dhiazfathra/go-pr-review-worker/issues/1/comments \
  --jq '[.[] | select(.body|startswith("<!-- pr-review-worker -->"))] | length'
```

```
3
```

## Persisted state

```bash
sqlite3 /tmp/prw-live/prw.db 'select id,state,substr(head_sha,1,8),event from jobs; select cycle,budget_notice_posted from pr_reviews;'
```

```
1|done|7e289d7b|opened
2|done|a481309c|synchronize
4|done|80c6f79e|synchronize
2|1
```

`cycle` stayed at 2 while a third job ran to completion, and
`budget_notice_posted` flipped to 1 — the flag that makes every later push
silent ([ADR-0004](../../adr/0004-two-review-cycles-per-pull-request.md)).

The missing job id 3 is itself evidence: that delivery was a redelivery for the
already-queued head `a481309`, so it was rejected at intake with `HTTP 200` and
never became a job.

```json
{
  "level": "INFO",
  "msg": "duplicate delivery ignored",
  "delivery": "github:dhiazfathra/go-pr-review-worker#1:a481309cc138d377c648d6d2750388d30107c77c"
}
```

That is the idempotency key doing its job
([ADR-0005](../../adr/0005-head-sha-idempotency-key.md)): the head SHA had not
changed yet when the delivery was replayed, so the worker correctly treated it
as the same state rather than spending the second cycle twice.

## What this does not prove

Restart-survival of the budget is covered by a test
(`TestCycleBudgetSurvivesWorkerRestart`) rather than by this live run: the
worker process here was never restarted mid-PR. The persistence itself is
visible above — the counter lives in SQLite, not in memory.

## Cleanup

```bash
pkill -f 'bin/pr-review-worker'
rm -rf /tmp/prw-live
```
