package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Verdict is the engine's judgement on one previously reported finding, after
// reading the author's reply and the commits that followed it.
type Verdict string

const (
	// VerdictFixed means the new code actually resolves the finding. The
	// thread is resolved and nothing further is posted.
	VerdictFixed Verdict = "fixed"
	// VerdictPartial means the change addresses part of the finding. The
	// thread stays open and the engine's note is posted as a reply.
	VerdictPartial Verdict = "partial"
	// VerdictUnfixed means the finding still stands, whatever the reply
	// claimed. Treated exactly like partial: reply, leave open.
	VerdictUnfixed Verdict = "unfixed"
	// VerdictUnrelated means the thread is not one the worker can judge from
	// this diff. It is left untouched, without a reply, because guessing here
	// would either close a real problem or spam a resolved conversation.
	VerdictUnrelated Verdict = "unrelated"
)

var verdicts = map[Verdict]bool{
	VerdictFixed:     true,
	VerdictPartial:   true,
	VerdictUnfixed:   true,
	VerdictUnrelated: true,
}

// OpenThread is one unresolved conversation handed to the engine for a verdict.
type OpenThread struct {
	// ID is the caller's handle for the thread. It is echoed back in the
	// verdict and never interpreted by the engine.
	ID string
	// File and Line are where the original finding was anchored.
	File string
	Line int
	// Finding is the body of the worker's original comment.
	Finding string
	// Replies are what humans said afterwards, oldest first.
	Replies []string
}

// ThreadVerdict is the engine's answer for one OpenThread.
type ThreadVerdict struct {
	ID      string  `json:"id"`
	Verdict Verdict `json:"verdict"`
	// Note explains the verdict. It is posted as a reply for anything that is
	// not fixed, so it is written for the pull request author to read.
	Note string `json:"note"`
}

// VerifyRequest asks an engine whether the follow-up commits fixed what an
// earlier pass reported.
type VerifyRequest struct {
	Repo     string
	PRNumber int
	Title    string
	// Diff is what changed since the reviewed commit — the evidence for or
	// against each reply's claim.
	Diff    string
	Threads []OpenThread
}

// VerifyResult is the parsed verdict set for one verification pass.
type VerifyResult struct {
	Verdicts []ThreadVerdict `json:"verdicts"`
	// Engine names the adapter that produced the result.
	Engine string `json:"-"`
}

// Verifier is an Engine that can also judge whether past findings were fixed.
// It is a separate interface so an engine adapter that only reviews still
// satisfies Engine.
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
}

// verifyPromptTemplate mirrors promptTemplate's contract: schema first,
// everything else forbidden.
const verifyPromptTemplate = `You are re-checking your own earlier code review comments on a pull request.

For each open thread below, decide whether the diff that followed actually fixes it.
Judge the DIFF, not the reply: a reply saying "fixed" with no matching code change is NOT fixed.

Output contract (violating it makes your answer unusable):
- Reply with exactly one JSON object and nothing else. No prose, no markdown fence.
- Schema:
  {"verdicts": [{"id": "<thread id, copied exactly from the input>",
                 "verdict": "fixed|partial|unfixed|unrelated",
                 "note": "<markdown, max 120 words, addressed to the author>"}]}
- One entry per thread, using the id exactly as given. Do not invent ids.
- "fixed": the diff fully resolves the finding. The note is ignored, keep it short.
- "partial": the diff addresses some of it. The note MUST say what is still missing.
- "unfixed": the finding still stands. The note MUST say why, citing the code.
- "unrelated": the diff gives you no evidence either way. The note is ignored.
- Be strict. Resolving a real problem because a reply claimed it was handled is
  the worst outcome here; leaving a thread open costs the author one more look.

Repository: %s
Pull request #%d: %s

Open threads:
%s
Diff since the reviewed commit:
%s
`

// buildVerifyPrompt renders the verification instruction.
func buildVerifyPrompt(req VerifyRequest) string {
	var threads strings.Builder

	for _, t := range req.Threads {
		fmt.Fprintf(&threads, "\n--- thread id: %s\nlocation: %s:%d\nyour original comment:\n%s\n", t.ID, t.File, t.Line, t.Finding)

		if len(t.Replies) == 0 {
			threads.WriteString("replies: (none)\n")

			continue
		}

		for i, r := range t.Replies {
			fmt.Fprintf(&threads, "reply %d:\n%s\n", i+1, r)
		}
	}

	return fmt.Sprintf(
		verifyPromptTemplate,
		req.Repo,
		req.PRNumber,
		req.Title,
		threads.String(),
		req.Diff,
	)
}

// parseVerifyResult reads an engine's verdicts, dropping anything that does not
// name a thread that was actually asked about. An engine that hallucinates a
// thread id must not cause a resolve call against an unknown handle.
func parseVerifyResult(out string, asked []OpenThread) (VerifyResult, error) {
	raw, err := extractJSON(out)
	if err != nil {
		return VerifyResult{}, err
	}

	var res VerifyResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return VerifyResult{}, fmt.Errorf("decoding engine verify json: %w", err)
	}

	known := make(map[string]bool, len(asked))
	for _, t := range asked {
		known[t.ID] = true
	}

	kept := make([]ThreadVerdict, 0, len(res.Verdicts))
	seen := make(map[string]bool, len(res.Verdicts))

	for _, v := range res.Verdicts {
		if !known[v.ID] || seen[v.ID] {
			continue
		}

		if !verdicts[v.Verdict] {
			// An unrecognised verdict is treated as "no evidence" rather than
			// guessed at, so a typo can never resolve a real finding.
			v.Verdict = VerdictUnrelated
		}

		seen[v.ID] = true

		kept = append(kept, v)
	}

	res.Verdicts = kept

	return res, nil
}
