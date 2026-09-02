# ADR-0014: Let the worker force the Claude engine's model explicitly

## Status

Accepted (2026-09-02)

## Context

The `claude` CLI picks a model from ambient state when the review prompt on
stdin carries no override: an inherited `ANTHROPIC_MODEL` environment
variable, or, failing that, the model recorded in the CLI's own persisted
`~/.claude/settings.json`. `internal/reviewer/cli.go`'s `childEnvAllowlist`
already forwards `HOME` and `XDG_CONFIG_HOME` — required for subscription-login
credentials to resolve via the OS keyring (see the README's "Engine
environment" section) — which means the CLI can read that settings file
regardless of what the worker's own configuration says.

On 2026-09-02 a manual run against a real PR dead-lettered after 3 attempts
with `404 not_found_error: model: claude-3-7-sonnet-20250219` — a retired
dated model id left over in a `~/.claude/settings.json` `"model"` alias
belonging to the account the CLI authenticated as. `PRW_CLAUDE_ARGS` had no
explicit `--model` (the CLI version in use does not even expose that flag),
and no `ANTHROPIC_MODEL` was set in the invoking environment, so the CLI
silently fell through to the stale account setting. Full writeup:
[docs/incidents/2026-09-02-manual-run-stale-model-alias.md](../incidents/2026-09-02-manual-run-stale-model-alias.md).

Every job attempt against a broken model burns one of `PRW_MAX_ATTEMPTS`
before the dead-letter comment fires, so this failure mode isn't just
noisy — it exhausts the retry budget on something no amount of retrying can
fix.

## Decision

Add `PRW_CLAUDE_MODEL` (optional, empty by default). When set, the `claude`
engine (`reviewer.CLI.Model`) forces `ANTHROPIC_MODEL` in the subprocess
environment to that value, unconditionally overriding whatever the invoking
shell exported (`internal/reviewer/cli.go:childEnv`). The OpenCode fallback
engine is unaffected — it doesn't read `ANTHROPIC_MODEL`.

This stays opt-in rather than defaulting to a hardcoded model id: model names
change over time, and a compiled-in default would itself go stale exactly
like the settings file did. The deployment is expected to set
`PRW_CLAUDE_MODEL` once, the same way it sets `PRW_CLAUDE_BIN`.

## Alternatives considered

### Document "export ANTHROPIC_MODEL before starting the worker"

- Pros: no code change.
- Cons: this is exactly the state that caused the incident — an unset
  `ANTHROPIC_MODEL` in the invoking shell falls straight through to the
  account's own settings file, so the failure mode reappears the moment
  someone starts the worker from a shell that doesn't happen to have it set
  (e.g. a fresh terminal, a different account, systemd with a trimmed env).
  Rejected: doesn't fix the underlying "silent fallback to stale config" gap.

### Strip `HOME`/`XDG_CONFIG_HOME` from the engine's environment instead

- Pros: the CLI could no longer read `~/.claude/settings.json` at all.
- Cons: subscription-login credentials are resolved via the OS keyring keyed
  by the account, which needs `HOME` to work (documented in the README).
  Removing it breaks the "no API key" deployment path entirely, trading one
  outage for a guaranteed one. Rejected.

### Hardcode a default model id in the binary

- Pros: works out of the box with no configuration.
- Cons: model ids are retired on a timeline the worker's release cadence
  cannot track; a compiled-in default becomes exactly the kind of stale
  reference this ADR exists to stop happening. Rejected.

## Consequences

- A deployment that sets `PRW_CLAUDE_MODEL` can no longer be silently
  redirected to a stale model by the service account's own `claude` settings,
  regardless of who or what last logged that account in interactively.
- A deployment that leaves it unset keeps today's behavior (inherit from
  environment, then from the CLI's settings file) — this ADR mitigates the
  failure mode without forcing a value on every operator.
- One more environment variable to document and to get right during setup;
  covered by `TestCLIModelOverridesInheritedAnthropicModel` and the README's
  "Engine environment" section.
