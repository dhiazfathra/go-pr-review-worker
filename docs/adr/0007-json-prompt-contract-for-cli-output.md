# ADR-0007: Get structured findings out of a conversational CLI with a strict JSON contract

## Status

Accepted (2026-09-01)

## Context

Claude Code and OpenCode are built for interactive and agentic use. Their
stdout is prose intended for humans: it may open with a greeting, wrap output in
a markdown fence, or append a closing remark. The worker needs
`(file, line, severity, title, body)` tuples to post inline comments.

## Decision

The prompt _is_ the contract (`internal/reviewer/prompt.go`):

- Reply with exactly one JSON object, no prose, no fence.
- Schema is stated inline, including that `line` must be a line the diff adds or
  modifies.
- Scope rules: defects only, no style opinions the linter covers, max N
  findings, empty findings is a valid answer.

Parsing is deliberately tolerant of the CLI ignoring the "no prose" part:
`extractJSON` scans every balanced, string-literal-respecting JSON object in
the output and picks the last one that carries a `findings` or `summary` key,
falling back to the last balanced object overall. An agentic CLI often prints
a status/telemetry object before the payload, so "last" beats "first"; a `}`
inside a comment body cannot end a scan early. Findings without a file or a
title are dropped (they cannot be anchored); an unknown severity degrades to
`minor` rather than failing the whole pass.

The prompt is delivered on **stdin**, never as an argv element, so a
megabyte-scale diff cannot hit `ARG_MAX`.

## Alternatives considered

### The CLI's own structured output mode (`--output-format json`)

- Pros: machine-readable envelope.
- Cons: the envelope is about the _session_ (turns, cost, tool calls); the
  review still arrives as free text inside it. It also differs between the two
  engines, so the parser would need to be per-engine.
- Rejected: the schema has to be specified in the prompt regardless, at which
  point the envelope adds a second format to track.

### Ask for one finding per line in a fixed text format

- Pros: trivially parseable, no brace balancing.
- Cons: multi-line finding bodies (the useful part) need escaping, which
  reintroduces the same problem badly.
- Rejected.

### Give the CLI tools and let it post comments itself

- Pros: no parsing at all.
- Cons: hands API write credentials to a model-driven loop, makes the review
  budget and comment dedup unenforceable, and makes failures unattributable.
- Rejected: the worker must stay the only thing that writes to the PR.

## Consequences

- Adding a third engine costs one `CLI` value with different args, not a new
  parser.
- If a model ignores the schema entirely, the job fails and retries; it never
  posts garbage.
- A finding citing a line outside the diff is rejected by the forge; the worker
  logs it and the finding still reaches the author through the summary comment.
