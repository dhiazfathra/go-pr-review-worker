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

```
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

Everything that makes those three lines hold — persistence, idempotency, dedup,
timeouts — is recorded in [`docs/adr/`](docs/adr/).

## Guarantees

| Property | How it holds |
|---|---|
| One job at a time | A single `Worker.Run` goroutine; it claims the next job only after finishing the current one ([ADR-0003](docs/adr/0003-single-worker-global-fifo.md)) |
| Global FIFO | `ORDER BY jobs.id`, across every repository and PR |
| Max 2 passes per PR | `pr_reviews.cycle`, keyed by `provider:repo#number`, in SQLite — survives restart ([ADR-0004](docs/adr/0004-two-review-cycles-per-pull-request.md)) |
| Redelivery-safe | Idempotency key is `provider:repo#number:head-sha`, not the delivery UUID ([ADR-0005](docs/adr/0005-head-sha-idempotency-key.md)) |
| No duplicate comments | `sha256(file+title)` fingerprints in `posted_comments` ([ADR-0011](docs/adr/0011-comment-dedup-by-fingerprint.md)) |
| A hung CLI cannot block the queue | Per-invocation timeout, then `SIGTERM`/`SIGKILL` to the whole process group ([ADR-0009](docs/adr/0009-engine-timeout-and-process-group-kill.md)) |
| Failures are never silent | Retry, then dead-letter with a comment on the PR ([ADR-0012](docs/adr/0012-failure-handling-retry-dead-letter-note.md)) |
| Unverified payloads never reach the queue | HMAC-SHA256 (GitHub) / constant-time token (GitLab), checked before parsing ([ADR-0010](docs/adr/0010-verify-webhook-signatures-before-enqueue.md)) |

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
`application/json`, secret = `PRW_GITHUB_WEBHOOK_SECRET`, event: *Pull
requests*), or a GitLab one at `/webhook/gitlab` (secret token =
`PRW_GITLAB_WEBHOOK_SECRET`, trigger: *Merge request events*).

For local testing without exposing a port, replay a real delivery against the
running worker. The payload is fetched from the live API and signed exactly as
GitHub signs it, so the verification path is exercised rather than bypassed:

```bash
PRW_GITHUB_WEBHOOK_SECRET=... scripts/deliver.sh owner/repo 7 opened
PRW_GITHUB_WEBHOOK_SECRET=... scripts/deliver.sh owner/repo 7 synchronize
```

With a public URL, `gh webhook forward --repo=owner/repo --events=pull_request
--url=http://localhost:8080/webhook/github` works too.

### Engine environment

The worker passes its own environment to the engine, so anything the CLI reads
is configured on the worker's process. In particular, an older Claude Code
install may be pinned to a retired model and fail every invocation with
`404 not_found_error: model: ...`; set the model on the worker:

```bash
ANTHROPIC_MODEL=claude-sonnet-5 ./bin/pr-review-worker
```

## Commands

| Command | Description |
|---|---|
| `make build` | Build `bin/pr-review-worker` |
| `make test` | `go test ./...` |
| `make cover` | Tests with a coverage summary |
| `make race` | Tests under the race detector |
| `make lint` | `golangci-lint run` |
| `make fmt` | `gofmt -w` over the tree |
| `make check` | fmt check + vet + lint + race tests |
| `make run` | Build and run with the current environment |

## Configuration

All configuration is environment variables; there is no config file.

| Variable | Default | Purpose |
|---|---|---|
| `PRW_ADDR` | `:8080` | HTTP listen address |
| `PRW_DB` | `prw.db` | SQLite path (queue + review budget) |
| `PRW_GITHUB_TOKEN` | — | PAT or app token with `pull_requests: write` |
| `PRW_GITHUB_WEBHOOK_SECRET` | — | HMAC secret for `/webhook/github` |
| `PRW_GITHUB_API` | `https://api.github.com` | Change for GitHub Enterprise |
| `PRW_GITLAB_TOKEN` | — | Token with `api` scope |
| `PRW_GITLAB_WEBHOOK_SECRET` | — | Secret token for `/webhook/gitlab` |
| `PRW_GITLAB_API` | `https://gitlab.com/api/v4` | Change for self-managed |
| `PRW_CLAUDE_BIN` | `claude` | Primary engine binary |
| `PRW_CLAUDE_ARGS` | `--print --output-format text` | Headless-mode flags |
| `PRW_OPENCODE_BIN` | `opencode` | Fallback engine binary |
| `PRW_OPENCODE_ARGS` | `run` | Headless-mode flags |
| `PRW_ENGINE_TIMEOUT` | `10m` | Kill an invocation after this |
| `PRW_MAX_CYCLES` | `2` | Review passes per PR |
| `PRW_MAX_ATTEMPTS` | `3` | Attempts before dead-lettering |
| `PRW_RETRY_DELAY` | `30s` | Wait before retrying a failed job |
| `PRW_POLL_INTERVAL` | `30s` | Idle wake-up period |
| `PRW_MIN_SEVERITY` | `minor` | `critical` \| `major` \| `minor` \| `nit` |
| `PRW_MAX_COMMENTS` | `20` | Inline comments posted per pass |
| `PRW_MAX_FINDINGS` | `25` | Findings requested from the engine |
| `PRW_ANNOUNCE_BUDGET_EXHAUSTED` | `true` | Post the one-time budget notice |
| `PRW_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |

At least one provider must have **both** a token and a webhook secret, or the
worker refuses to start.

## Architecture

```
cmd/pr-review-worker      wiring, HTTP server, graceful shutdown
internal/webhook          thin intake adapter: verify, parse, enqueue
internal/store            SQLite: jobs, pr_reviews, posted_comments
internal/worker           the review loop: budget, diff scoping, posting
internal/reviewer         Engine interface, CLI adapter, prompt contract, Chain
internal/provider         GitHub and GitLab REST clients (six calls each)
internal/config           environment loading and validation
```

The seams that matter:

- **`webhook` → `store`** — the handler verifies and enqueues, nothing more. All
  ordering, dedup and budget logic lives behind the queue.
- **`worker` → `reviewer.Engine`** — the worker never names a binary. Claude,
  OpenCode and the test fake are interchangeable.
- **`worker` → `provider.Provider`** — GitHub and GitLab differ only in an
  adapter; the worker's logic is forge-agnostic.

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
    {"file": "internal/store/store.go", "line": 142, "severity": "major",
     "title": "stable one-line title", "body": "what is wrong, why, and the fix"}
  ]
}
```

Prose around the object is tolerated: the parser extracts the first balanced
JSON object, respecting string literals. Findings with no file or no title are
dropped rather than posted as comments on nothing.

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
and how "detect rate limited" is defined precisely.
