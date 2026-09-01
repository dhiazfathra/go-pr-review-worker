# Evidence: the worker reviews a live PR on open, and again on the next push

Task: end-to-end run of the review worker against
[dhiazfathra/go-pr-review-worker#1](https://github.com/dhiazfathra/go-pr-review-worker/pull/1),
reviewing the very branch that implements it (commits `7e289d7` and `a481309`).

The worker ran on localhost against the **real** GitHub API with a real token.
Only the webhook transport was local: `scripts/deliver.sh` fetches the live PR
payload and signs it exactly as GitHub does, so the HMAC verification path is
exercised rather than bypassed.

## Setup

```bash
PRW_GITHUB_TOKEN="$(gh auth token)" \
PRW_GITHUB_WEBHOOK_SECRET=live-secret-abc123 \
PRW_DB=/tmp/prw-live/prw.db \
PRW_ADDR=127.0.0.1:8099 \
PRW_ENGINE_TIMEOUT=15m \
ANTHROPIC_MODEL=claude-sonnet-5 \
./bin/pr-review-worker > /tmp/prw-live/worker.log 2>&1 &

curl -s http://127.0.0.1:8099/healthz
```

```
{"status":"ok","pending_jobs":0}
```

`ANTHROPIC_MODEL` is required here because the locally installed Claude Code
(0.2.115) is pinned to a retired model and otherwise fails every invocation:

```
API Error: 404 {"type":"error","error":{"type":"not_found_error","message":"model: claude-3-7-sonnet-20250219"}}
```

That is recorded because it is exactly the class of failure the engine
environment note in the README now covers.

## Pass 1 — `pull_request: opened`

```bash
PRW_GITHUB_WEBHOOK_SECRET=live-secret-abc123 \
  scripts/deliver.sh dhiazfathra/go-pr-review-worker 1 opened
```

```
HTTP 202
```

Worker log (full log: [`worker.log`](./worker.log)):

```json
{"level":"INFO","msg":"job enqueued","delivery":"github:dhiazfathra/go-pr-review-worker#1:7e289d7bc7c92f6d593fbffcde2742f91588a4c5","event":"opened","head":"7e289d7bc7c92f6d593fbffcde2742f91588a4c5"}
{"level":"INFO","msg":"job started","job":1,"attempt":1}
{"level":"INFO","msg":"engine finished","job":1,"engine":"claude","findings":3}
{"level":"INFO","msg":"comments posted","job":1,"candidates":2,"fresh":2,"posted":2}
{"level":"INFO","msg":"job done","job":1,"duration":"3m16.154990917s"}
```

What landed on the PR:

```bash
gh api repos/dhiazfathra/go-pr-review-worker/pulls/1/comments \
  --jq '.[] | "\(.path):\(.line) — \(.body | split("\n")[2])"'
```

```
internal/worker/worker.go:343 — **🟠 major** — Comment fingerprint claimed as posted before the API call succeeds
internal/provider/gitlab.go:174 — **🟡 minor** — PostInline re-fetches the merge request on every finding (N+1 calls)
```

Summary comment (excerpt, `gh api .../issues/1/comments`):

```
<!-- pr-review-worker -->

## Automated review — pass 1 of 2

... inline-comment fingerprints are recorded as "posted" in `posted_comments`
*before* the provider API call that actually posts them succeeds, so a transient
posting failure (network blip, 5xx, timeout) permanently suppresses that finding
from ever being posted again ...

### 2 inline comment(s)

- 🟠 major `internal/worker/worker.go:343` — Comment fingerprint claimed as posted before the API call succeeds
- 🟡 minor `internal/provider/gitlab.go:174` — PostInline re-fetches the merge request on every finding (N+1 calls)

<sub>engine: `claude`</sub>
```

Note the numbers: the engine produced **3** findings, **2** passed the severity
threshold and the dedup filter, **2** were posted. The third appears in the
summary prose only — the filter is doing its job, not silently dropping work.

Both findings were real defects in code written in this session. The major one
was a genuine bug: `ClaimFingerprints` recorded the fingerprint before
`PostInline` was attempted, so a transient 5xx suppressed the finding forever.
Fixed in `a481309` (`UnseenFingerprints` + `RecordFingerprint`), with a
regression test that fails against the old behaviour.

## Pass 2 — `pull_request: synchronize`, scoped to the new commits

```bash
git push                       # a481309: the fixes for pass 1's findings
PRW_GITHUB_WEBHOOK_SECRET=live-secret-abc123 \
  scripts/deliver.sh dhiazfathra/go-pr-review-worker 1 synchronize
```

```
HTTP 202
```

```json
{"level":"INFO","msg":"job enqueued","delivery":"github:dhiazfathra/go-pr-review-worker#1:a481309cc138d377c648d6d2750388d30107c77c","event":"synchronize","head":"a481309cc138d377c648d6d2750388d30107c77c"}
{"level":"INFO","msg":"job started","job":2,"attempt":1}
{"level":"INFO","msg":"engine finished","job":2,"engine":"claude","findings":2}
{"level":"INFO","msg":"comments posted","job":2,"candidates":2,"fresh":2,"posted":2}
{"level":"INFO","msg":"job done","job":2,"duration":"1m32.227559666s"}
```

**1m32s against 3m16s for pass 1** — the incremental diff
([ADR-0006](../../adr/0006-incremental-diff-on-the-second-pass.md)) is doing
what it exists for: pass 2 reviewed the delta since `7e289d7`, not the whole
~5000-line PR again.

The pass-2 summary confirms it read the delta, and it found two new defects in
the pass-1 fixes themselves:

```
## Automated review — pass 2 of 2

Pass-2 diff fixes two real defects from pass 1: fingerprints are now recorded
only after a successful `PostInline` ... The new GitLab diff-refs memoisation
cache, however, is unbounded for the life of the process ...

- 🟠 major `internal/provider/gitlab.go:22` — Diff-refs cache grows unbounded for the life of the process
- 🟡 minor `internal/store/store.go:324` — UnseenFingerprints issues one query per fingerprint

_Final pass. Later pushes to this pull request will not be reviewed._
```

Neither pass-1 finding was re-posted, because both fingerprints were recorded
after their successful post ([ADR-0011](../../adr/0011-comment-dedup-by-fingerprint.md)).

## Persisted state after both passes

```bash
sqlite3 -header -column /tmp/prw-live/prw.db 'select id,state,attempts,substr(head_sha,1,12) head,event from jobs;'
sqlite3 -header /tmp/prw-live/prw.db 'select * from pr_reviews;'
```

```
id  state  attempts  head          event
--  -----  --------  ------------  -----------
1   done   1         7e289d7bc7c9  opened
2   done   1         a481309cc138  synchronize

pr_key|cycle|last_reviewed_sha|summary_comment_id|summary_cycle|budget_notice_posted|updated_at
github:dhiazfathra/go-pr-review-worker#1|2|a481309cc138d377c648d6d2750388d30107c77c|5489424093|2|0|2026-09-01T05:38:17.709332Z
```

Both jobs succeeded on the first attempt. `cycle = 2` means the budget is now
spent — which is the precondition for the third-push evidence in
[`review-budget-is-enforced.md`](./review-budget-is-enforced.md).

## Cleanup

```bash
pkill -f 'bin/pr-review-worker'
rm -rf /tmp/prw-live
```

The database lived under `/tmp` and was never committed; `prw.db*` is gitignored.
