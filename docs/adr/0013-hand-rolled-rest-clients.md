# ADR-0013: Hand-roll the forge REST clients instead of vendoring SDKs

## Status

Accepted (2026-09-01)

## Context

The worker needs six operations per forge: read PR metadata, read the PR diff,
compare two commits, post an inline comment, post a comment, edit a comment.
`go-github` and `go-gitlab` each cover hundreds of endpoints.

## Decision

`internal/provider` implements those six calls over `net/http` behind one
`Provider` interface, with a shared `httpClient` that sets auth, bounds the
response at 8 MB — reading one byte past the cap so an oversized body fails
loudly with an explicit error instead of silently reviewing a truncated diff —
and turns any non-2xx into an error carrying the status and a 200-character
body snippet.

## Alternatives considered

### `google/go-github` + `xanzy/go-gitlab`

- Pros: maintained, typed, pagination and rate-limit helpers included.
- Cons: two large dependency trees and a second update surface, for six calls;
  the diff media type and GitLab's `position[...]` discussion form still need
  hand-holding.
- Rejected: "a little copying is better than a little dependency" applies
  squarely at this call count.

### `gh` / `glab` CLIs as subprocesses

- Rejected: a second class of external binary to install and version, for
  something `net/http` does in a few lines.

## Consequences

- Zero third-party dependencies outside the SQLite driver.
- Pagination is not implemented, because none of the six calls needs it. Adding
  a listing endpoint later would.
- Both clients are covered by `httptest` stubs asserting the exact wire details
  that are easy to get wrong: the diff `Accept` header, GitHub's `side: RIGHT`,
  GitLab's full `base/start/head` position triple.
- GitHub Enterprise and self-managed GitLab work by changing a base URL.
