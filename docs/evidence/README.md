# Evidence

Executed proof for claims made in a task, PR, or session — kept as artifacts,
not just prose in a chat transcript. "Verified, not claimed": this folder is
where the verification is preserved, so a reviewer (or a later agent) can check
it without re-running everything.

## Structure

One folder per task, numbered sequentially, tracked next to the code it verifies:

```
docs/evidence/
├── README.md                       this file
├── 0001-<task-slug>/
│   ├── <finding>.md                narrative + exact commands + full output
│   └── <supporting artifact>       raw logs, screenshots, recordings
└── 0002-<next-task-slug>/
```

- **Number** — monotonically increasing, never reused or reordered.
- **Slug** — short, matches the task or commit it backs.
- **One `.md` per claim**, not one file per task. Someone checking "does the
  cycle budget hold" should not have to wade through unrelated build logs.

## Contents

- **[0001-review-worker-end-to-end](./0001-review-worker-end-to-end/)** — the
  first live run, on this repository's own
  [PR #1](https://github.com/dhiazfathra/go-pr-review-worker/pull/1): two
  review passes driven by signed webhook deliveries, and the third push that
  gets a budget notice instead of a review.
- **[0002-watcher-and-reply-verification](./0002-watcher-and-reply-verification/)**
  — the watcher ([ADR-0015](../adr/0015-watch-repositories-instead-of-trusting-webhooks.md))
  and the follow-up pass ([ADR-0016](../adr/0016-verify-replies-against-the-diff-before-resolving.md)),
  run against a throwaway PR on a repository with no webhook at all. Includes
  the defect the run found: a forge that refuses an approval used to fail the
  whole job.

## What goes in each `.md`

```markdown
# Evidence: <claim being verified>

Task: <what this backs> (commit `<sha>`).

## Command run

<exact command, copy-pasteable>

## Output

<full output, not a paraphrase — including exit codes and HTTP statuses>

## Cleanup

<teardown, if the verification stood up state>
```

Rules:

- Paste real output from a command actually run. Never an example written from
  memory or "what it should look like".
- State exit codes and HTTP status codes explicitly, not "it worked".
- Record failures and surprises too, with the explanation. Evidence that a
  design decision holds under a failure is worth more than a clean happy path.
- Screenshots or video only where text cannot carry the claim (rendering, an
  interactive flow). A `curl` body already proves a JSON response.
