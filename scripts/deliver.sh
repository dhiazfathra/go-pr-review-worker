#!/usr/bin/env bash
# Replay a real pull_request webhook delivery against a locally running worker.
#
#   scripts/deliver.sh <owner/repo> <pr number> <opened|synchronize> [url] [secret]
#
# The payload is built from the live GitHub API, so the head SHA, base SHA and
# repository name are the real ones — only the transport is local. The signature
# is computed exactly as GitHub computes it, so the worker's verification path is
# exercised rather than bypassed.
set -euo pipefail

repo="${1:?owner/repo}"
number="${2:?pr number}"
action="${3:-opened}"
url="${4:-http://127.0.0.1:8099/webhook/github}"
secret="${5:-${PRW_GITHUB_WEBHOOK_SECRET:?set PRW_GITHUB_WEBHOOK_SECRET}}"

# shellcheck disable=SC2016 # $action is a jq variable bound by --arg, not a shell one
payload="$(gh api "repos/${repo}/pulls/${number}" | jq -c --arg action "$action" \
	'{action: $action,
	  number: .number,
	  pull_request: {number: .number, head: {sha: .head.sha}, base: {sha: .base.sha}},
	  repository: {full_name: .base.repo.full_name}}')"

signature="sha256=$(printf '%s' "$payload" |
	openssl dgst -sha256 -hmac "$secret" -hex | awk '{print $NF}')"

printf '%s' "$payload" | curl -sS -o /dev/stderr -w 'HTTP %{http_code}\n' \
	-X POST "$url" \
	-H 'Content-Type: application/json' \
	-H 'X-GitHub-Event: pull_request' \
	-H "X-GitHub-Delivery: $(uuidgen)" \
	-H "X-Hub-Signature-256: ${signature}" \
	--data-binary @-
