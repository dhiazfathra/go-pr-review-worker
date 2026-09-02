# Evidence: a refused approval no longer fails the job

Task: a defect found by the sandbox run in
[`replies-are-judged-against-the-diff.md`](./replies-are-judged-against-the-diff.md),
and its fix.

This is the one thing the exercise found that the unit tests, the live run on
`DermaestheticsGroup/dermaesthetics-new-backend#111`, and
[ADR-0016](../../adr/0016-verify-replies-against-the-diff-before-resolving.md)
all missed — because #111 never reached the approve call at all (a thread was
left open), so the call had never once been made against a real forge.

## The failure

Job 5 on the sandbox PR: budget spent, both threads verified fixed and
resolved, nothing new found, so `approve` fired. `dhiazfathra` was both the PR
author and the token's account:

```text
{"level":"INFO","msg":"threads verified","job":5,"engine":"claude","considered":2,"resolved":2,"still_open":0}
{"level":"WARN","msg":"job failed, will retry","job":5,"attempt":1,"error":"approving pull request: POST /repos/dhiazfathra/prw-sandbox/pulls/1/reviews: unexpected status 422: {\"message\":\"Unprocessable Entity\",\"errors\":[\"Review Can not approve your own pull request\"],\"documentation_url\":\"https://docs.github.com/rest/pulls/reviews#create-a-review-for-a-pull-request\",\"status\"","retry_in":"30s"}
```

The review had **succeeded** — comments posted, threads resolved — and the job
was marked failed anyway, because `approve` returned its error and `review()`
propagated it.

## Why that is worse than a noisy log

The refusal is permanent. GitHub returns `422` whenever the token's account
opened the pull request, and `403` when the account cannot review the
repository. Neither changes on retry, so:

1. `PRW_MAX_ATTEMPTS=3` retries a call that can never succeed.
2. The job dead-letters and the worker posts _"Automated review failed after 3
   attempts and will not be retried"_ — on a pull request whose findings were
   posted and whose threads were resolved.
3. `state.LastReviewedSHA` was never saved, because the error aborted before
   `SavePRState`, so the next push re-verifies the same threads.

The author is told the review failed when it did not. That is the same class of
false failure notice as the
[2026-09-02 stale-model incident](../../incidents/2026-09-02-manual-run-stale-model-alias.md),
reached by a different route.

It also contradicted this repository's own ADR-0016, which claimed: _"A token
that cannot approve fails that call only; the resolves still stand."_ The code
did not do that.

The dead-letter comment was avoided here only because the worker was killed
during the 30s retry backoff:

```bash
gh api repos/dhiazfathra/prw-sandbox/issues/1/comments --jq '.[].body' | grep -c failed
```

```text
0
```

## The fix

A **refusal** is logged and the pull request left unapproved; the job still
succeeds. A **transient** failure still fails the job, because retrying it is
the right thing to do — nothing about a timeout, a `429` or a `502` says the
approval is disallowed. The two are told apart by the status code, via a typed
`provider.StatusError`:

```go
if err := tr.Approve(ctx, job.Repo, job.PRNumber, body); err != nil {
	var status *provider.StatusError
	if errors.As(err, &status) && status.Permanent() {
		log.Warn("approving pull request refused, leaving it unapproved", "error", err)

		return false, nil
	}

	return false, fmt.Errorf("approving pull request: %w", err)
}
```

The first version of this fix swallowed **every** approval error, which traded
one bug for another: a `502` would have been recorded as "permanently
unapproved" and never retried. The worker's own re-review of PR #2 caught that
(`internal/worker/verify.go` thread), which is why the classification exists.

## The regression test, shown to fail without the fix

`TestARefusedApprovalLeavesTheJobSuccessful` sets `approveErr` to a
`*provider.StatusError` with code `422` and relies on `runJob`, which fails the test if the queue does not
drain — and `PendingCount` counts `failed` jobs, so a job left in retry hangs
it.

A test that passes for the wrong reason proves nothing, so the old behaviour
was restored temporarily to confirm the test actually reaches the approve path:

```bash
# with `panic("OLD BEHAVIOUR: ...")` in place of the new log-and-continue
go test ./internal/worker/ -run TestARefusedApprovalLeavesTheJobSuccessful
```

```text
panic: OLD BEHAVIOUR: job would fail here: 422 Can not approve your own pull request
FAIL	github.com/dhiazfathra/go-pr-review-worker/internal/worker	0.616s
```

```bash
# fix restored
go test ./internal/worker/ -run TestARefusedApprovalLeavesTheJobSuccessful
```

```text
ok  	github.com/dhiazfathra/go-pr-review-worker/internal/worker	0.342s
```

## Full suite after the change

```bash
go test ./... && go vet ./... && golangci-lint run
```

```text
ok  	github.com/dhiazfathra/go-pr-review-worker/cmd/pr-review-worker	1.847s
ok  	github.com/dhiazfathra/go-pr-review-worker/internal/config	(cached)
ok  	github.com/dhiazfathra/go-pr-review-worker/internal/provider	(cached)
ok  	github.com/dhiazfathra/go-pr-review-worker/internal/reviewer	(cached)
ok  	github.com/dhiazfathra/go-pr-review-worker/internal/store	(cached)
ok  	github.com/dhiazfathra/go-pr-review-worker/internal/webhook	(cached)
ok  	github.com/dhiazfathra/go-pr-review-worker/internal/worker	1.578s
0 issues.
```

## Confirmation against the real forge

The same job, re-run with the fixed binary and a reviewer account that did not
open the PR, approved successfully — see
[`replies-are-judged-against-the-diff.md`](./replies-are-judged-against-the-diff.md).
The refusal path is covered by the unit test rather than re-provoked live,
because provoking it again means deliberately dead-lettering a pull request.
`TestATransientApprovalFailureFailsTheJob` covers the other side, with a `503`.
