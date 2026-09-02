# go-pr-review-worker

An automated code-review agent for pull requests and merge requests. A webhook
arrives, a job is queued, and a single worker drives an agentic coding CLI
(Claude Code, with OpenCode as an automatic fallback) in headless mode to
produce inline comments and a summary — which the worker posts through the
GitHub or GitLab REST API.

One binary, one SQLite file, no broker, no database server. It is designed for a
2 vCPU / 4 GB VM where the expensive process is the review CLI, not the daemon
supervising it.

## How a review happens

```text
PR opened ──▶ POST /webhook/github ──▶ verify HMAC ──▶ enqueue (SQLite)
                                                          │
                                          ┌───────────────┘
                                          ▼
                       one worker goroutine, one job at a time
                                          │
                    ┌─────────────────────┼─────────────────────┐
                    ▼                     ▼                     ▼
              fetch diff            claude CLI            post inline
           (full or delta)     (opencode on 429)      comments + summary
```

1. `pull_request: opened` (or GitLab `open`) enqueues review pass 1. The worker
   reviews the **full** PR diff.
2. `pull_request: synchronize` (or GitLab `update` carrying `oldrev`) enqueues
   pass 2. The worker reviews only the diff **since the last reviewed commit**.
3. Any push after that gets one "review budget exhausted" comment, then silence.

Before each pass — and after the budget is spent — the worker re-checks the
threads it already opened against the new commits, resolving the ones that are
genuinely fixed and objecting again on the ones that are not. See
[Follow-up passes](#follow-up-passes-verifying-replies).

A webhook is the fast path, not the only one: `PRW_WATCH_REPOS` polls for
pushes no webhook delivered — because the worker was down, or the repository
has no hook at all ([ADR-0015](docs/adr/0015-watch-repositories-instead-of-trusting-webhooks.md)).

Everything that makes those lines hold — persistence, idempotency, dedup,
timeouts — is recorded in [`docs/adr/`](docs/adr/).

## Guarantees

| Property                                      | How it holds                                                                                                                                                                                                                                                                |
| --------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| One job at a time                             | A single `Worker.Run` goroutine; it claims the next job only after finishing the current one ([ADR-0003](docs/adr/0003-single-worker-global-fifo.md))                                                                                                                       |
| Global FIFO                                   | `ORDER BY jobs.id`, across every repository and PR                                                                                                                                                                                                                          |
| Max 2 passes per PR                           | `pr_reviews.cycle`, keyed by `provider:repo#number`, in SQLite — survives restart ([ADR-0004](docs/adr/0004-two-review-cycles-per-pull-request.md))                                                                                                                         |
| Redelivery-safe                               | Idempotency key is `provider:repo#number:head-sha`, not the delivery UUID ([ADR-0005](docs/adr/0005-head-sha-idempotency-key.md))                                                                                                                                           |
| No duplicate comments                         | `sha256(file+title)` fingerprints in `posted_comments` ([ADR-0011](docs/adr/0011-comment-dedup-by-fingerprint.md))                                                                                                                                                          |
| A hung CLI cannot block the queue             | Per-invocation timeout, then `SIGTERM`/`SIGKILL` to the whole process group ([ADR-0009](docs/adr/0009-engine-timeout-and-process-group-kill.md))                                                                                                                            |
| Failures are never silent                     | Retry, then dead-letter with a comment on the PR ([ADR-0012](docs/adr/0012-failure-handling-retry-dead-letter-note.md))                                                                                                                                                     |
| Unverified payloads never reach the queue     | HMAC-SHA256 (GitHub) / constant-time token (GitLab), checked before parsing ([ADR-0010](docs/adr/0010-verify-webhook-signatures-before-enqueue.md))                                                                                                                         |
| A missed webhook is still reviewed            | The watcher compares each open PR's head against `last_reviewed_sha` and enqueues under the same idempotency key ([ADR-0015](docs/adr/0015-watch-repositories-instead-of-trusting-webhooks.md))                                                                             |
| A thread is resolved on evidence, not a claim | The engine judges the diff since the last review, not the author's reply; anything short of "fixed" stays open, and a "fixed" verdict for a file the diff never touched is refused outright ([ADR-0016](docs/adr/0016-verify-replies-against-the-diff-before-resolving.md)) |
| The worker only closes its own threads        | Ownership is the forge's record of who wrote the first comment, not a marker in its body ([ADR-0016](docs/adr/0016-verify-replies-against-the-diff-before-resolving.md))                                                                                                    |

## Quick start

```bash
git clone https://github.com/dhiazfathra/go-pr-review-worker
cd go-pr-review-worker
make build                          # -> bin/pr-review-worker

cp .env.example .env                # fill in tokens and webhook secrets
set -a && . ./.env && set +a
./bin/pr-review-worker
```

Point a GitHub webhook at `https://your-host/webhook/github` (content type
`application/json`, secret = `PRW_GITHUB_WEBHOOK_SECRET`, event: _Pull
requests_), or a GitLab one at `/webhook/gitlab` (secret token =
`PRW_GITLAB_WEBHOOK_SECRET`, trigger: _Merge request events_).

For local testing without exposing a port, replay a real delivery against the
running worker. The payload is fetched from the live API and signed exactly as
GitHub signs it, so the verification path is exercised rather than bypassed:

```bash
PRW_GITHUB_WEBHOOK_SECRET=... scripts/deliver.sh owner/repo 7 opened
PRW_GITHUB_WEBHOOK_SECRET=... scripts/deliver.sh owner/repo 7 synchronize
```

`gh webhook forward` works too — `gh extension install cli/gh-webhook` once,
then the command below. Its `--url` stays `localhost`: the CLI holds the
connection open, so this is local testing as well, not a public endpoint. Pass
the secret explicitly, because without `--secret` the hook is created without
one, the worker rejects every delivery with `401`, and nothing is enqueued.

```bash
gh webhook forward --repo=owner/repo --events=pull_request \
  --secret="$PRW_GITHUB_WEBHOOK_SECRET" \
  --url=http://localhost:8080/webhook/github
```

### Engine environment

The engine does **not** inherit the worker's environment. Contributor-controlled
diff content reaches the engine, so it is spawned with an allowlist only —
`PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_ALL`, `TMPDIR`, the `XDG_*`
directories, `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` and
`ANTHROPIC_MODEL`. Forge tokens and webhook secrets are never passed down.

Authentication therefore has to arrive through one of the allowlisted paths:

- **Subscription login** (`claude` already logged in) works with no extra
  configuration. The credentials live in the OS keyring, which is why `HOME`,
  `USER` and `LOGNAME` are on the allowlist — the keyring lookup is keyed by
  the account name, and without them every invocation fails with
  `Invalid API key · Please run /login` on a host where the CLI works fine
  interactively.

  Those credentials are **per account**, so the login has to be done as the
  account the unit runs under, not as your own. With the sample unit that is
  `pr-review-worker`, which has `/usr/sbin/nologin` as its shell:

  ```bash
  sudo -u pr-review-worker HOME=/var/lib/pr-review-worker -s /bin/sh -c 'claude /login'
  ```

  Logging in as yourself leaves the service failing with the same
  `Invalid API key` message while `claude` works in your own shell.

- **API key**: set `ANTHROPIC_API_KEY` on the worker's process. This is the
  simpler option for an unattended deployment, since it needs no interactive
  login as the service account.

Anything else the CLI reads has to be added to `childEnvAllowlist` in
`internal/reviewer/cli.go` deliberately.

`claude` also reads its own persisted `~/.claude/settings.json`, which
`HOME`/`XDG_CONFIG_HOME` let it see, and that file can pin a model
independently of any environment variable. If it names a retired dated model,
every invocation dead-letters with `404 not_found_error: model: ...` even
though the worker's own environment looks fine — this happened in
[docs/incidents/2026-09-02-manual-run-stale-model-alias.md](docs/incidents/2026-09-02-manual-run-stale-model-alias.md).
Set `PRW_CLAUDE_MODEL` so the worker forces `ANTHROPIC_MODEL` for every
`claude` invocation regardless of what the account's settings file or
invoking shell happen to have:

```bash
PRW_CLAUDE_MODEL=claude-sonnet-5 ./bin/pr-review-worker
```

Prefer this over exporting `ANTHROPIC_MODEL` directly: an unset
`ANTHROPIC_MODEL` silently falls through to whatever the CLI's own settings
pick, while `PRW_CLAUDE_MODEL` always wins.

## Commands

| Command      | Description                                |
| ------------ | ------------------------------------------ |
| `make build` | Build `bin/pr-review-worker`               |
| `make test`  | `go test ./...`                            |
| `make cover` | Tests with a coverage summary              |
| `make race`  | Tests under the race detector              |
| `make lint`  | `golangci-lint run`                        |
| `make fmt`   | `gofmt -w` over the tree                   |
| `make check` | fmt check + vet + lint + race tests        |
| `make run`   | Build and run with the current environment |

## Configuration

All configuration is environment variables; there is no config file.

| Variable                        | Default                        | Purpose                                                                                                                                                         |
| ------------------------------- | ------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PRW_ADDR`                      | `:8080`                        | HTTP listen address                                                                                                                                             |
| `PRW_DB`                        | `prw.db`                       | SQLite path (queue + review budget)                                                                                                                             |
| `PRW_GITHUB_TOKEN`              | —                              | PAT or app token with `pull_requests: write`                                                                                                                    |
| `PRW_GITHUB_WEBHOOK_SECRET`     | —                              | HMAC secret for `/webhook/github`                                                                                                                               |
| `PRW_GITHUB_API`                | `https://api.github.com`       | Change for GitHub Enterprise                                                                                                                                    |
| `PRW_GITLAB_TOKEN`              | —                              | Token with `api` scope                                                                                                                                          |
| `PRW_GITLAB_WEBHOOK_SECRET`     | —                              | Secret token for `/webhook/gitlab`                                                                                                                              |
| `PRW_GITLAB_API`                | `https://gitlab.com/api/v4`    | Change for self-managed                                                                                                                                         |
| `PRW_ALLOW_INSECURE_LOOPBACK`   | `false`                        | Permit a plaintext loopback forge endpoint                                                                                                                      |
| `PRW_CLAUDE_BIN`                | `claude`                       | Primary engine binary                                                                                                                                           |
| `PRW_CLAUDE_ARGS`               | `--print --output-format text` | Headless-mode flags                                                                                                                                             |
| `PRW_CLAUDE_MODEL`              | —                              | Forces `ANTHROPIC_MODEL` for the `claude` engine, overriding any value inherited from the invoking shell or the CLI's own `~/.claude/settings.json` (see below) |
| `PRW_OPENCODE_BIN`              | `opencode`                     | Fallback engine binary                                                                                                                                          |
| `PRW_OPENCODE_ARGS`             | `run`                          | Headless-mode flags                                                                                                                                             |
| `PRW_ENGINE_TIMEOUT`            | `10m`                          | Kill an invocation after this                                                                                                                                   |
| `PRW_MAX_CYCLES`                | `2`                            | Review passes per PR                                                                                                                                            |
| `PRW_MAX_ATTEMPTS`              | `3`                            | Attempts before dead-lettering                                                                                                                                  |
| `PRW_RETRY_DELAY`               | `30s`                          | Wait before retrying a failed job                                                                                                                               |
| `PRW_POLL_INTERVAL`             | `30s`                          | Idle wake-up period                                                                                                                                             |
| `PRW_MIN_SEVERITY`              | `minor`                        | `critical` \| `major` \| `minor` \| `nit`                                                                                                                       |
| `PRW_MAX_COMMENTS`              | `20`                           | Inline comments posted per pass                                                                                                                                 |
| `PRW_MAX_FINDINGS`              | `25`                           | Findings requested from the engine                                                                                                                              |
| `PRW_ANNOUNCE_BUDGET_EXHAUSTED` | `true`                         | Post the one-time budget notice                                                                                                                                 |
| `PRW_WATCH_REPOS`               | —                              | Comma-separated `provider:owner/name` list polled for pushes no webhook delivered; empty disables the watcher                                                   |
| `PRW_WATCH_INTERVAL`            | `2m`                           | How often the watcher re-lists open pull requests                                                                                                               |
| `PRW_VERIFY_REPLIES`            | `true`                         | Re-check the worker's own open threads against the new commits and resolve the ones actually fixed                                                              |
| `PRW_APPROVE_WHEN_RESOLVED`     | `false`                        | Submit an approving review once every thread the worker opened is resolved and the pass found nothing new                                                       |
| `PRW_LOG_LEVEL`                 | `info`                         | `debug` \| `info` \| `warn` \| `error`                                                                                                                          |

At least one provider must have **both** a token and a webhook secret, or the
worker refuses to start.

### Watching repositories

A webhook that never arrives is indistinguishable from a review that found
nothing. Set `PRW_WATCH_REPOS` and the worker also polls, comparing each open
pull request's head against what it last reviewed:

```bash
PRW_WATCH_REPOS="github:octocat/hello,github:octocat/world" \
PRW_WATCH_INTERVAL=2m \
./bin/pr-review-worker
```

This catches a push that landed while the worker was down, a repository with no
hook configured, and a hook whose secret was rotated. It is **not** a
replacement for webhooks — they react in seconds, the watcher in minutes — and
the two are safe to run together: both build the same
`provider:repo#number:head-sha` delivery id, so a push seen by both becomes one
job, not two. Drafts are skipped, and a head already reviewed is never
requeued.

Watching a forge whose credentials are missing is a startup error rather than a
poll that fails forever. See
[ADR-0015](docs/adr/0015-watch-repositories-instead-of-trusting-webhooks.md).

### Follow-up passes: verifying replies

The worker reads the conversations it started. On every job — including after
the two-pass budget is spent — it takes the **unresolved** threads whose first
comment is its own, and asks the engine to judge them against the diff since
the last reviewed commit **and** the author's replies:

| Verdict     | What happens                                                          |
| ----------- | --------------------------------------------------------------------- |
| `fixed`     | Thread resolved, with a short confirming reply                        |
| `partial`   | Thread stays open; the engine's note is posted saying what is missing |
| `unfixed`   | Thread stays open; the engine's note is posted saying why             |
| `unrelated` | Thread untouched — no evidence either way is not a verdict            |

The rule the prompt enforces is that a reply is a claim, not evidence: **a
comment saying "fixed in abc123" with no matching code change is not fixed.**
A verdict naming a thread that was never asked about is dropped, and an
unrecognised verdict is downgraded to `unrelated` — neither can resolve a real
finding.

Verifying does not consume a review cycle; answering the author about findings
already reported is not a new review.

With `PRW_APPROVE_WHEN_RESOLVED=true` the worker submits an approving review
once every thread it opened is resolved and the same pass found nothing new.
This is **off by default**: an approval can satisfy branch protection and
unblock a merge, so it stays an explicit decision. It happens at most once per
pull request.

The token's account must be allowed to review the repository and must not be
the pull request's author — GitHub answers
`422 Can not approve your own pull request`. A refused approval is logged and
the pull request is left unapproved; the review itself still succeeds.

Resolving a thread is a GitHub GraphQL mutation with no REST equivalent, so
this pass runs on **GitHub only**; on GitLab the worker logs that it is skipped
and reviews as before. See
[ADR-0016](docs/adr/0016-verify-replies-against-the-diff-before-resolving.md).

## Architecture

```text
cmd/pr-review-worker      wiring, HTTP server, graceful shutdown
internal/webhook          thin intake adapter: verify, parse, enqueue
internal/store            SQLite: jobs, pr_reviews, posted_comments
internal/worker           the review loop: budget, diff scoping, posting,
                          the follow-up pass, and the repository watcher
internal/reviewer         Engine/Verifier interfaces, CLI adapter, prompts, Chain
internal/provider         GitHub and GitLab REST clients, plus GitHub GraphQL
                          for review threads
internal/config           environment loading and validation
```

The seams that matter:

- **`webhook` → `store`** — the handler verifies and enqueues, nothing more. All
  ordering, dedup and budget logic lives behind the queue.
- **`worker.Watcher` → `store`** — the watcher is a second producer for the same
  queue, using the same idempotency key, so it never competes with the webhook
  path ([ADR-0015](docs/adr/0015-watch-repositories-instead-of-trusting-webhooks.md)).
- **`worker` → `reviewer.Engine`** — the worker never names a binary. Claude,
  OpenCode and the test fake are interchangeable.
- **`worker` → `provider.Provider`** — GitHub and GitLab differ only in an
  adapter; the worker's logic is forge-agnostic.
- **`provider.ThreadReviewer`** — the optional capability (read, reply, resolve,
  approve) that the follow-up pass needs. GitHub satisfies it; GitLab does not,
  and the worker skips that pass rather than calling methods a forge cannot
  honour ([ADR-0016](docs/adr/0016-verify-replies-against-the-diff-before-resolving.md)).

A visual walkthrough of the module boundaries lives in
[`docs/architecture-review.html`](docs/architecture-review.html).

### CLI invocation contract

The engine is a conversational tool, so the prompt makes the schema the task
(see [ADR-0007](docs/adr/0007-json-prompt-contract-for-cli-output.md)). The
prompt and the diff go on **stdin**; the reply is expected to be one JSON object:

```json
{
  "summary": "markdown, max 200 words",
  "findings": [
    {
      "file": "internal/store/store.go",
      "line": 142,
      "severity": "major",
      "title": "stable one-line title",
      "body": "what is wrong, why, and the fix"
    }
  ]
}
```

Prose around the object is tolerated: the parser extracts a balanced JSON
object from the output, respecting string literals ([ADR-0007](docs/adr/0007-json-prompt-contract-for-cli-output.md)).
Findings with no file or no title are dropped rather than posted as comments
on nothing.

The follow-up pass uses a second contract with the same shape — one JSON
object, one entry per thread it was asked about:

```json
{
  "verdicts": [
    {
      "id": "thread id, copied exactly from the input",
      "verdict": "fixed|partial|unfixed|unrelated",
      "note": "markdown addressed to the author, max 120 words"
    }
  ]
}
```

A verdict whose `id` was not in the request is discarded, and an unrecognised
`verdict` becomes `unrelated`, so neither a hallucinated id nor a typo can
resolve a live finding.

### Rate-limit fallback

The worker spawns Claude Code first. It switches to OpenCode **only** when the
invocation's output matches a rate-limit signature (`usage limit reached`,
`429`, `529`, `overloaded_error`, `quota exceeded`, `retry-after`, …). Any other
failure returns immediately, because it would fail identically on the fallback.
The exact list and its rationale:
[ADR-0008](docs/adr/0008-rate-limit-detection-and-fallback.md).

## Operations

`GET /healthz` → `{"status":"ok","pending_jobs":3}`

Inspect state directly; the schema is small on purpose:

```bash
sqlite3 prw.db 'SELECT id, repo, pr_number, head_sha, state, attempts, last_error FROM jobs ORDER BY id DESC LIMIT 10;'
sqlite3 prw.db 'SELECT * FROM pr_reviews;'
```

Requeue a dead-lettered job (a deliberate operator action):

```bash
sqlite3 prw.db "UPDATE jobs SET state='queued', attempts=0 WHERE id=42;"
```

Reset a PR's review budget:

```bash
sqlite3 prw.db "DELETE FROM pr_reviews WHERE pr_key='github:owner/repo#7';"
```

Deploy with the sample unit in [`deploy/pr-review-worker.service`](deploy/pr-review-worker.service).

## Testing

```bash
make check
```

Statement coverage is 93%+ across the tree, and the tests are behavioural rather
than structural: the engine adapter is exercised against real spawned processes
(including one that traps `SIGTERM`, to prove the kill policy works), the
provider clients against `httptest` stubs asserting the exact wire details, and
the worker against a fake forge that records everything a PR author would see.

Executed proof for specific claims lives in [`docs/evidence/`](docs/evidence/).

## Decisions

Every significant choice is an ADR in [`docs/adr/`](docs/adr/), including the
language comparison that led to Go, why SQLite is both the queue and the budget,
how "detect rate limited" is defined precisely, why the worker polls as well as
listening ([ADR-0015](docs/adr/0015-watch-repositories-instead-of-trusting-webhooks.md)),
and why a reply claiming a fix is never enough to resolve a thread
([ADR-0016](docs/adr/0016-verify-replies-against-the-diff-before-resolving.md)).

Incidents and their follow-up actions are in
[`docs/incidents/`](docs/incidents/).
