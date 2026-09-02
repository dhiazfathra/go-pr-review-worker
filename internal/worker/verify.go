package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dhiazfathra/go-pr-review-worker/internal/provider"
	"github.com/dhiazfathra/go-pr-review-worker/internal/reviewer"
	"github.com/dhiazfathra/go-pr-review-worker/internal/store"
)

// verifyOutcome is what one follow-up pass did, for the caller to log and to
// decide whether approving is warranted.
type verifyOutcome struct {
	// Considered is how many of the worker's own threads were still open.
	Considered int
	// Resolved is how many the engine judged fixed and the forge accepted.
	Resolved int
	// Open is how many remain unresolved after the pass.
	Open int
	// Ran reports whether the pass actually happened; false means it was
	// skipped (disabled, unsupported provider, or no prior reviewed commit).
	Ran bool
	// ForeignOpen is how many unresolved threads belong to somebody else. The
	// worker never touches them, but it must not approve over the top of them
	// either: a human objection is still an objection.
	ForeignOpen int
}

// verify re-checks the worker's own unresolved threads against the commits that
// followed them. A thread the engine judges fixed is resolved; anything else
// gets the engine's note as a reply and stays open, so a claim of "fixed" in a
// reply never closes a finding on its own.
//
// It deliberately does not consume a review cycle: answering the author about
// findings already reported is not a new review pass, and charging the budget
// for it would mean a PR that gets a follow-up never receives its second pass.
func (w *Worker) verify(
	ctx context.Context,
	job store.Job,
	prov provider.Provider,
	state store.PRState,
	pr provider.PullRequest,
	log *slog.Logger,
) (verifyOutcome, error) {
	if !w.cfg.VerifyReplies {
		return verifyOutcome{}, nil
	}

	tr, ok := prov.(provider.ThreadReviewer)
	if !ok {
		log.Info("provider cannot resolve threads, skipping verification", "provider", prov.Name())

		return verifyOutcome{}, nil
	}

	verifier, ok := w.engine.(reviewer.Verifier)
	if !ok {
		log.Info("engine cannot verify, skipping verification")

		return verifyOutcome{}, nil
	}

	threads, err := tr.ReviewThreads(ctx, job.Repo, job.PRNumber)
	if err != nil {
		return verifyOutcome{}, fmt.Errorf("listing review threads: %w", err)
	}

	// The evidence is what landed since the reviewed commit. Without a prior
	// reviewed SHA there is nothing to compare against and every verdict would
	// be a guess, so the pass is skipped rather than run blind. This is checked
	// before the threads are counted: it is the difference between "could not
	// run" and "ran and found nothing to do".
	if state.LastReviewedSHA == "" || state.LastReviewedSHA == job.HeadSHA {
		return verifyOutcome{}, nil
	}

	open, byID := openWorkerThreads(threads)
	foreign := foreignOpenThreads(threads)

	if len(open) == 0 {
		// Nothing open is a result, not a skip: every thread the worker opened
		// is already resolved (or it never opened one). Reporting Ran here is
		// what lets a pull request whose last thread was closed on an earlier
		// pass still be approved — `out.Open` is 0 and the condition
		// PRW_APPROVE_WHEN_RESOLVED describes is satisfied. No engine call is
		// made, because there is nothing to ask about.
		return verifyOutcome{Ran: true, ForeignOpen: foreign}, nil
	}

	diff, err := prov.CompareDiff(ctx, job.Repo, state.LastReviewedSHA, job.HeadSHA)
	if err != nil {
		log.Warn("verification diff unavailable, skipping", "error", err)

		return verifyOutcome{}, nil
	}

	res, err := verifier.Verify(ctx, reviewer.VerifyRequest{
		Repo:     job.Repo,
		PRNumber: job.PRNumber,
		Title:    pr.Title,
		Diff:     diff,
		Threads:  open,
	})
	if err != nil {
		return verifyOutcome{}, fmt.Errorf("verifying threads: %w", err)
	}

	out := verifyOutcome{Considered: len(open), Ran: true, ForeignOpen: foreign}
	touched := changedPaths(diff)

	for _, v := range res.Verdicts {
		t, ok := byID[v.ID]
		if !ok {
			continue
		}

		if v.Verdict == reviewer.VerdictFixed {
			// The engine reads text the pull request's author controls — the
			// diff and the replies — so a `fixed` verdict is not authority on
			// its own; an instruction smuggled into either could ask for one.
			// The forge's own file list is the deterministic check: if the
			// commits since the last review never touched the file the finding
			// is on, nothing there can have been fixed, whatever the verdict
			// says. This cannot catch a misjudged real change, but it does
			// mean closing a finding always requires a matching code change.
			if !touched[t.Path] {
				log.Warn(
					"ignoring a fixed verdict for a file the diff does not touch",
					"thread", t.ID,
					"path", t.Path,
				)

				continue
			}

			if err := tr.ResolveThread(ctx, t.ID); err != nil {
				// A thread that cannot be resolved is left open rather than
				// reported as fixed; the next pass will try again.
				log.Warn("resolving thread failed", "thread", t.ID, "error", err)

				continue
			}

			if err := tr.ReplyToThread(ctx, job.Repo, job.PRNumber, lastCommentID(t), renderResolved(res.Engine)); err != nil {
				// The thread is already resolved; a missing courtesy reply is
				// not worth failing the job over.
				log.Warn("posting resolution reply failed", "thread", t.ID, "error", err)
			}

			out.Resolved++

			continue
		}

		if v.Verdict == reviewer.VerdictUnrelated {
			continue
		}

		if err := tr.ReplyToThread(
			ctx, job.Repo, job.PRNumber, lastCommentID(t), renderFollowUp(v, res.Engine),
		); err != nil {
			log.Warn("posting follow-up reply failed", "thread", t.ID, "error", err)
		}
	}

	out.Open = out.Considered - out.Resolved

	log.Info(
		"threads verified",
		"engine", res.Engine,
		"considered", out.Considered,
		"resolved", out.Resolved,
		"still_open", out.Open,
	)

	return out, nil
}

// changedPaths reads the file names out of a unified diff, taking **both**
// sides of each hunk header.
//
// The post-image (`+++ b/path`) is the name a review comment is anchored to,
// so it is the one that usually matches. The pre-image (`--- a/path`) matters
// for the two cases where the thread's path no longer exists in the new tree:
// a file that was **deleted** (which trivially fixes any finding on it) and one
// that was **renamed** (where the thread still carries the old name). Reading
// only the post-image left both looking untouched, so a correct `fixed` verdict
// was discarded and the thread stayed open forever.
//
// `/dev/null` is skipped: it is the absent side of an add or a delete, not a
// path.
func changedPaths(diff string) map[string]bool {
	paths := make(map[string]bool)

	for _, line := range strings.Split(diff, "\n") {
		var prefix string

		switch {
		case strings.HasPrefix(line, "+++ "):
			prefix = "b/"
		case strings.HasPrefix(line, "--- "):
			prefix = "a/"
		default:
			continue
		}

		p := strings.TrimSpace(line[4:])
		if p == "/dev/null" {
			continue
		}

		paths[strings.TrimPrefix(p, prefix)] = true
	}

	return paths
}

// openWorkerThreads selects the unresolved threads this worker started and
// renders them for the engine. A thread opened by a human is not the worker's
// to resolve, and one already resolved needs no verdict.
func openWorkerThreads(threads []provider.ReviewThread) ([]reviewer.OpenThread, map[string]provider.ReviewThread) {
	open := make([]reviewer.OpenThread, 0, len(threads))
	byID := make(map[string]provider.ReviewThread, len(threads))

	for _, t := range threads {
		if t.Resolved || len(t.Comments) == 0 {
			continue
		}

		// Both conditions are needed. The marker says the comment came from a
		// worker; the forge's authorship says it came from *this* account. A
		// marker is just text in a body, so anyone could open a thread the
		// worker would then treat as its own and resolve.
		if !t.StartedByWorker || !strings.Contains(t.Comments[0].Body, summaryMarker) {
			continue
		}

		ot := reviewer.OpenThread{
			ID:      t.ID,
			File:    t.Path,
			Line:    t.Line,
			Finding: t.Comments[0].Body,
		}

		for _, c := range t.Comments[1:] {
			// The worker's own follow-up replies are not evidence about
			// whether the code was fixed; feeding them back would let it
			// re-read its own opinion as corroboration.
			if strings.Contains(c.Body, summaryMarker) {
				continue
			}

			ot.Replies = append(ot.Replies, c.Author+": "+c.Body)
		}

		open = append(open, ot)
		byID[t.ID] = t
	}

	return open, byID
}

// foreignOpenThreads counts unresolved threads the worker did not open. They
// are nobody's business but their author's, and approving while one is open
// would overrule a reviewer the worker cannot even read a verdict for.
func foreignOpenThreads(threads []provider.ReviewThread) int {
	var n int

	for _, t := range threads {
		if t.Resolved || len(t.Comments) == 0 {
			continue
		}

		if t.StartedByWorker && strings.Contains(t.Comments[0].Body, summaryMarker) {
			continue
		}

		n++
	}

	return n
}

// lastCommentID picks the comment to reply to. GitHub threads a reply by the
// id of a comment already in the conversation, and the first one is guaranteed
// to exist for the life of the thread.
func lastCommentID(t provider.ReviewThread) string {
	if len(t.Comments) == 0 {
		return ""
	}

	return t.Comments[0].ID
}

// renderResolved is the note left on a thread the worker is closing.
func renderResolved(engine string) string {
	return fmt.Sprintf(
		"%s\n\n✅ **Verified fixed** — the follow-up commits address this. Resolving.\n\n<sub>engine: `%s`</sub>\n",
		summaryMarker,
		engineName(engine),
	)
}

// renderFollowUp is the note left on a thread that is not fixed yet.
func renderFollowUp(v reviewer.ThreadVerdict, engine string) string {
	label := "❌ **Still open**"
	if v.Verdict == reviewer.VerdictPartial {
		label = "⚠️ **Partially addressed**"
	}

	note := strings.TrimSpace(v.Note)
	if note == "" {
		note = "_No detail produced._"
	}

	return fmt.Sprintf(
		"%s\n\n%s\n\n%s\n\n<sub>engine: `%s`</sub>\n",
		summaryMarker,
		label,
		note,
		engineName(engine),
	)
}

func engineName(engine string) string {
	if engine == "" {
		return "unknown"
	}

	return engine
}

// approve submits an approving review once every thread the worker opened is
// resolved and this pass added nothing new. It is the last step of a follow-up
// pass, never of a first review: approving a PR the worker has just commented
// on for the first time would contradict its own findings.
//
// A rejected approval is reported, not returned as an error. The forge refuses
// one for reasons that are permanent and none of them mean the review went
// wrong: a token whose account opened the pull request gets
// `422 Can not approve your own pull request`, and one without review
// permission gets `403`. Failing the job there would retry a call that can
// never succeed and then dead-letter a pull request whose findings were posted
// and whose threads were resolved, telling the author the review failed when it
// did not.
func (w *Worker) approve(
	ctx context.Context,
	job store.Job,
	prov provider.Provider,
	state store.PRState,
	out verifyOutcome,
	newFindings int,
	log *slog.Logger,
) (bool, error) {
	switch {
	case !w.cfg.ApproveWhenResolved, !out.Ran, state.Approved:
		return false, nil
	case out.Open > 0 || newFindings > 0:
		return false, nil
	case out.ForeignOpen > 0:
		// Somebody else's conversation is still open. The worker has no
		// standing to resolve it and no business approving past it.
		log.Info("not approving: unresolved threads from other reviewers", "threads", out.ForeignOpen)

		return false, nil
	}

	tr, ok := prov.(provider.ThreadReviewer)
	if !ok {
		return false, nil
	}

	body := fmt.Sprintf(
		"%s\n\n## Automated review — approved\n\n"+
			"Every finding from the earlier pass is resolved and this pass found nothing new.\n",
		summaryMarker,
	)

	if err := tr.Approve(ctx, job.Repo, job.PRNumber, body); err != nil {
		var status *provider.StatusError
		if errors.As(err, &status) && status.Permanent() {
			log.Warn("approving pull request refused, leaving it unapproved", "error", err)

			return false, nil
		}

		// A timeout, a 429 or a 5xx says nothing about whether the approval is
		// allowed, so the job fails and the retry tries again. The comments and
		// resolves already stand and are idempotent.
		return false, fmt.Errorf("approving pull request: %w", err)
	}

	log.Info("pull request approved", "resolved", out.Resolved)

	return true, nil
}
