# Evidence: the watcher reviews a push no webhook ever delivered

Task: verify [ADR-0015](../../adr/0015-watch-repositories-instead-of-trusting-webhooks.md)
against a repository that has **no webhook at all** — the failure mode that
left `DermaestheticsGroup/dermaesthetics-new-backend#111` with a half-finished
review on 2026-09-02.

The sandbox is a throwaway repository created for this run,
[dhiazfathra/prw-sandbox#1](https://github.com/dhiazfathra/prw-sandbox/pull/1).
No hook was ever registered on it, and `PRW_ADDR` was bound to loopback, so the
watcher was the only path a job could take into the queue. `dhiazfathra` opened
the PR; the worker ran with that account's token for the first three jobs.

## Setup

```bash
gh repo create dhiazfathra/prw-sandbox --private --source=. --push
gh pr create --repo dhiazfathra/prw-sandbox --base main --head feat/percentile \
  --title "feat: add Percentile"

PRW_GITHUB_TOKEN="$(gh auth token -u dhiazfathra)" \
PRW_GITHUB_WEBHOOK_SECRET="$(openssl rand -hex 32)" \
PRW_DB=.../run1/prw.db \
PRW_ADDR=127.0.0.1:18111 \
PRW_WATCH_REPOS=github:dhiazfathra/prw-sandbox \
PRW_WATCH_INTERVAL=20s \
PRW_VERIFY_REPLIES=true \
PRW_APPROVE_WHEN_RESOLVED=true \
PRW_CLAUDE_MODEL=claude-sonnet-5 \
  ./pr-review-worker
```

`PRW_GITHUB_WEBHOOK_SECRET` is a throwaway value generated for this run. No
webhook was registered with it, so it granted nothing — it is only there
because GitHub is enabled by having both a token and a secret.

## A never-seen PR is enqueued as `opened`

The watcher's first sweep, 0.4s after startup:

```text
{"level":"INFO","msg":"watching repositories","count":1,"interval":"20s"}
{"level":"INFO","msg":"job enqueued by watcher","delivery":"github:dhiazfathra/prw-sandbox#1:570c88c5bc57b97f92dba711c1bbd5b6c6a4976a","pr":"github:dhiazfathra/prw-sandbox#1","event":"opened","head":"570c88c5bc57b97f92dba711c1bbd5b6c6a4976a"}
{"level":"INFO","msg":"engine finished","job":1,"engine":"claude","findings":4}
{"level":"INFO","msg":"comments posted","job":1,"candidates":4,"fresh":4,"posted":4,"unposted":0}
{"level":"INFO","msg":"job done","job":1,"duration":"22.039427625s"}
```

`"event":"opened"` is the point of ADR-0015's third property: the worker had
never seen this PR, so it reviews the whole diff rather than an increment
against a SHA it never reviewed.

## A later push is enqueued as `synchronize`

After a fix commit was pushed (`a3aa296`), with no webhook involved:

```text
{"level":"INFO","msg":"job enqueued by watcher","delivery":"github:dhiazfathra/prw-sandbox#1:a3aa296aa9af593ecd8258278c929919d6270c76","event":"synchronize","head":"a3aa296aa9af593ecd8258278c929919d6270c76"}
```

and again for `c8de70c`. Three pushes, three jobs, zero webhook deliveries.

## An already-reviewed head is skipped

Between those pushes the watcher swept every 20s and produced **no log lines at
all** — `last_reviewed_sha == head` is compared before anything reaches the
queue (ADR-0015 property 2), so a quiet repository costs one
`GET /pulls?state=open` per interval and no queue churn.

## The delivery id is the webhook path's id

```text
github:dhiazfathra/prw-sandbox#1:570c88c5bc57b97f92dba711c1bbd5b6c6a4976a
```

`provider:repo#number:head-sha` ([ADR-0005](../../adr/0005-head-sha-idempotency-key.md)),
identical to what the webhook handler builds, which is what makes the two
mechanisms deduplicate rather than double-review.

This was also confirmed the hard way. Requeueing the third head by hand after
a failure did **not** produce a second job, because `INSERT OR IGNORE` on that
delivery id was a no-op — the row already existed. The dedup is real, not
theoretical.

## Full log

[`sandbox-run.log`](./sandbox-run.log)

## Cleanup

The worker was killed and its SQLite database left outside the repository
(`/private/tmp/.../run1/prw.db`). `dhiazfathra/prw-sandbox` is a throwaway
repository and can be deleted; nothing in this repository depends on it.
