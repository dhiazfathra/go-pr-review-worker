package reviewer

import (
	"fmt"
	"strings"
)

// promptTemplate is the CLI invocation contract. An agentic CLI is built for
// conversation, so the only way to get machine-readable output is to make the
// schema part of the task and to forbid everything around it.
const promptTemplate = `You are a code reviewer. Review the unified diff below and reply with JSON only.

Output contract (violating it makes your answer unusable):
- Reply with exactly one JSON object and nothing else. No prose, no markdown fence.
- Schema:
  {"summary": "<markdown, max 200 words>",
   "findings": [{"file": "<path exactly as in the diff>",
                 "line": <integer line number in the NEW file>,
                 "severity": "critical|major|minor|nit",
                 "title": "<one line, <=80 chars, stable wording>",
                 "body": "<markdown: what is wrong, why it matters, suggested fix>"}]}
- "line" MUST be a line the diff adds or modifies. Never comment on unchanged lines.
- Report only defects: correctness, security, data loss, concurrency, resource
  leaks, error handling, missing tests for new branches. No style opinions the
  linter already covers. No praise. Empty findings is a valid answer.
- Max %d findings, highest severity first.

Repository: %s
Pull request #%d: %s
%s
Unified diff to review:
%s
`

// buildPrompt renders the review instruction for one request.
func buildPrompt(req Request, maxFindings int) string {
	scope := ""
	if req.Cycle > 1 {
		scope = "Review pass 2 of 2: the diff below contains only what changed since the previous review. Do not re-report anything from the list above.\n"
	}

	prior := ""
	if len(req.PriorFindings) > 0 {
		prior = "Already reported in an earlier pass (do not repeat):\n- " +
			strings.Join(req.PriorFindings, "\n- ") + "\n"
	}

	body := ""
	if strings.TrimSpace(req.Body) != "" {
		body = "Description:\n" + req.Body + "\n"
	}

	return fmt.Sprintf(
		promptTemplate,
		maxFindings,
		req.Repo,
		req.PRNumber,
		req.Title,
		body+prior+scope,
		req.Diff,
	)
}
