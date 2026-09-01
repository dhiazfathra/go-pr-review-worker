# ADR-0010: Verify the delivery signature before anything else touches the payload

## Status

Accepted (2026-09-01)

## Context

The webhook endpoints are public. An unauthenticated caller who can enqueue jobs
can spend the agent quota, post comments as the worker's identity on any
repository it has a token for, and fill the queue.

## Decision

`internal/webhook` reads the body under a 2 MB limit, then verifies *before*
parsing or enqueueing:

- **GitHub**: HMAC-SHA256 over the raw body, compared against
  `X-Hub-Signature-256` with `subtle.ConstantTimeCompare`.
- **GitLab**: `X-Gitlab-Token` compared against the configured secret with
  `subtle.ConstantTimeCompare` (GitLab offers no HMAC mode, so the token is the
  entire authentication story and must be long and random).

A missing or empty configured secret rejects every delivery for that provider
rather than accepting all of them. `config.Load` refuses to start unless at
least one provider has both a token and a webhook secret.

## Alternatives considered

### `hmac.Equal` on decoded bytes

- Equivalent in security; the current form compares the full header string in
  constant time and rejects a wrong prefix explicitly. Kept for the clearer
  error.

### IP allowlisting the forge's published ranges

- Rejected as a substitute: the ranges change, and it authenticates the network
  path rather than the payload. Fine as defence in depth at the proxy.

### Trusting a shared reverse proxy to verify

- Rejected: the worker is the component that acts on the payload; it must be the
  component that verifies it.

## Consequences

- A forged delivery gets `401` and never reaches the queue — asserted by a test
  that checks the queue is still empty after a bad signature.
- Rotating a secret requires restarting the worker (configuration is read at
  startup).
- Body size is capped, so an oversized delivery fails verification rather than
  the allocator.
