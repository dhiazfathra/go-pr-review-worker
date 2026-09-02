# Evidence: threads are resolved on the diff, and a PR is approved only when all of them are

Task: verify [ADR-0016](../../adr/0016-verify-replies-against-the-diff-before-resolving.md)
end to end on
[dhiazfathra/prw-sandbox#1](https://github.com/dhiazfathra/prw-sandbox/pull/1),
the throwaway PR described in
[`watcher-enqueues-a-push-no-webhook-delivered.md`](./watcher-enqueues-a-push-no-webhook-delivered.md).

The PR added a `Percentile` helper with real defects: an index that reaches
`len(xs)` at `p == 100`, an in-place `sort.Float64s` on the caller's slice, and
no empty-input or range validation.

## Pass 1 — four findings

```
{"level":"INFO","msg":"engine finished","job":1,"engine":"claude","findings":4}
{"level":"INFO","msg":"comments posted","job":1,"candidates":4,"fresh":4,"posted":4,"unposted":0}
```

```bash
gh api repos/dhiazfathra/prw-sandbox/pulls/1/comments \
  --jq '.[] | "\(.id) \(.path):\(.line)"'
```

```
3911606887 stats.go:18   🔴 critical — Index out of range when p == 100
3911607031 stats.go:16   🟠 major    — Percentile mutates caller's slice via sort.Float64s
3911607118 stats.go:17   🟠 major    — No validation of p range or empty input
3911607211 stats.go:14   🟡 minor    — No tests added for Percentile
```

All four are genuine, and the critical one is a real panic.

## Fix, reply, push

`a3aa296` clamped the index, copied before sorting, added `ErrNoSamples` /
`ErrPercentileRange`, and added `stats_test.go`. Each thread got an author
reply naming the commit, e.g.:

```bash
gh api -X POST repos/dhiazfathra/prw-sandbox/pulls/1/comments/3911606887/replies \
  -f body="Clamped the index — see a3aa296."
```

## The follow-up pass resolves all four

```
{"level":"INFO","msg":"threads verified","job":3,"engine":"claude","considered":4,"resolved":4,"still_open":0}
```

Four thread ids in, four `fixed` verdicts back, four `resolveReviewThread`
mutations accepted. The replies claimed a fix **and the diff contained one**,
which is the case ADR-0016 wants resolved.

## Pass 2 found something new, so approval was withheld

The same job then ran its review pass on the incremental diff:

```
{"level":"INFO","msg":"engine finished","job":3,"engine":"claude","findings":2}
{"level":"INFO","msg":"comments posted","job":3,"candidates":2,"fresh":2,"posted":2,"unposted":0}
```

Both new findings are correct and neither existed before the fix:

```
3911621972 stats.go:33       🟡 minor — Percentile does not reject NaN for p
3911622092 stats_test.go:36  🟡 minor — No test for NaN percentile input
```

`p < 0 || p > 100` is false for `NaN`, because every comparison with `NaN` is
false, so `NaN` passed validation and reached `int(p / 100 * float64(len))`.

No `pull request approved` line was logged for job 3. `PRW_APPROVE_WHEN_RESOLVED`
was `true` and every old thread was resolved, so the gate that held was
`newFindings > 0` — approving and objecting in the same pass is incoherent.

```bash
sqlite3 run1/prw.db 'select cycle,last_reviewed_sha,approved from pr_reviews;'
```

```
2|a3aa296aa9af593ecd8258278c929919d6270c76|0
```

## The third push: verification with the budget spent

`c8de70c` added `math.IsNaN(p)` and a `math.NaN()` test case, with a reply on
each of the two new threads. `cycle` was already at `MaxCycles=2`, so no review
ran — but the follow-up did, which is ADR-0016's "the pass does not consume a
cycle and runs after the budget is spent":

```
{"level":"INFO","msg":"threads verified","job":5,"engine":"claude","considered":2,"resolved":2,"still_open":0}
{"level":"INFO","msg":"pull request approved","job":5,"resolved":2}
{"level":"INFO","msg":"job done","job":5,"duration":"19.992727417s"}
```

Six of six threads resolved, nothing new found, approval submitted:

```bash
gh api repos/dhiazfathra/prw-sandbox/pulls/1/reviews --jq '.[] | "\(.user.login) \(.state)"'
gh api graphql -f query='{repository(owner:"dhiazfathra",name:"prw-sandbox"){pullRequest(number:1){reviewThreads(first:20){nodes{isResolved path}}}}}' \
  --jq '.data.repository.pullRequest.reviewThreads.nodes[] | "resolved=\(.isResolved) \(.path)"'
sqlite3 run1/prw.db 'select cycle,last_reviewed_sha,approved from pr_reviews;'
```

```
dhiazfathra          COMMENTED   (×18)
dhiaz-dermaesthetics COMMENTED   (×2)
dhiaz-dermaesthetics APPROVED

resolved=true stats.go        (×5)
resolved=true stats_test.go

2|c8de70c84cf7a3f2a1ad60af8d58a83ffe518148|1
```

`approved=1` makes it once-only: a later push cannot approve again.

## Why the reviewer is a second account

GitHub answers `422 Can not approve your own pull request` when the token's
account opened the PR. `dhiazfathra` opened this one, so the approve path can
only be exercised by a different reviewer — the worker ran as
`dhiaz-dermaesthetics` (added as a collaborator) for job 5. That refusal was
first hit for real and is a defect in its own right; see
[`a-refused-approval-does-not-fail-the-job.md`](./a-refused-approval-does-not-fail-the-job.md).

To re-run job 5 under the second account, the two `c8de70c` threads were
un-resolved with the `unresolveReviewThread` mutation and
`pr_reviews.last_reviewed_sha` was rewound to `a3aa296`. That is state surgery
on a scratch database, not a code path — worth stating plainly rather than
presenting the second run as untouched.

## What this does _not_ prove

The `partial` and `unfixed` verdicts are not exercised here: every reply in this
run was backed by a real fix, so every verdict came back `fixed`. That branch
was exercised on `DermaestheticsGroup/dermaesthetics-new-backend#111`, where a
reply claiming "Fixed in 783950bc" was answered with
`⚠️ Partially addressed` and the thread was **left open**, because the diff
added a warning log instead of fixing array bodies. Unit tests cover it too
(`TestVerifyDoesNotResolveOnTheAuthorsWordAlone`).

## Full log

[`sandbox-run.log`](./sandbox-run.log)

## Cleanup

Both workers killed; scratch database outside the repository.
`dhiazfathra/prw-sandbox` is disposable, and `dhiaz-dermaesthetics` was added
to it as a collaborator solely to act as a reviewer.
