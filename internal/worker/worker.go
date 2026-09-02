// Package worker is the single-threaded review loop. It owns the review budget,
// diff scoping, engine dispatch, and comment posting; everything else in the
// program either feeds it jobs or is an adapter it calls.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/dhiazfathra/go-pr-review-worker/internal/provider"
	"github.com/dhiazfathra/go-pr-review-worker/internal/reviewer"
	"github.com/dhiazfathra/go-pr-review-worker/internal/store"
)

// Config tunes the loop. Zero values are replaced by the defaults in New.
type Config struct {
	// MaxCycles is the review budget per PR. The spec fixes it at 2.
	MaxCycles int
	// MaxAttempts is how many times a failing job is retried before it is
	// dead-lettered and reported on the PR.
	MaxAttempts int
	// RetryDelay is the wait before a failed job is retried.
	RetryDelay time.Duration
	// PollInterval is the idle wake-up period; the notify channel handles the
	// common case, this only covers a missed notification.
	PollInterval time.Duration
	// MinSeverity drops findings below this rank before posting.
	MinSeverity reviewer.Severity
	// MaxComments caps inline comments posted per cycle.
	MaxComments int
	// AnnounceBudgetExhausted posts a one-time note when a third push arrives.
	AnnounceBudgetExhausted bool
	// VerifyReplies re-checks the worker's own open threads against the new
	// commits and the author's replies before reviewing anything new.
	VerifyReplies bool
	// ApproveWhenResolved allows an approving review once every thread the
	// worker opened is resolved and the latest pass found nothing new.
	ApproveWhenResolved bool
}

// Worker runs review jobs one at a time, in FIFO order, forever.
type Worker struct {
	store     *store.Store
	engine    reviewer.Engine
	providers map[string]provider.Provider
	notify    <-chan struct{}
	cfg       Config
	log       *slog.Logger
}

// New builds a worker. providers is keyed by the provider name a job carries.
func New(
	st *store.Store,
	engine reviewer.Engine,
	providers map[string]provider.Provider,
	notify <-chan struct{},
	cfg Config,
	log *slog.Logger,
) *Worker {
	if cfg.MaxCycles == 0 {
		cfg.MaxCycles = 2
	}

	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}

	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 30 * time.Second
	}

	if cfg.PollInterval == 0 {
		cfg.PollInterval = 30 * time.Second
	}

	if cfg.MinSeverity == "" {
		cfg.MinSeverity = reviewer.SeverityMinor
	}

	if cfg.MaxComments == 0 {
		cfg.MaxComments = 20
	}

	return &Worker{
		store:     st,
		engine:    engine,
		providers: providers,
		notify:    notify,
		cfg:       cfg,
		log:       log,
	}
}

// Run drains the queue until ctx is cancelled. One job at a time, by
// construction: there is exactly one Run goroutine and it never forks.
func (w *Worker) Run(ctx context.Context) {
	for {
		job, err := w.store.ClaimNext(ctx)

		switch {
		case errors.Is(err, store.ErrNoJob):
			if !w.wait(ctx) {
				return
			}

			continue

		case err != nil:
			w.log.Error("claiming job failed", "error", err)

			if !w.wait(ctx) {
				return
			}

			continue
		}

		w.runJob(ctx, job)

		if ctx.Err() != nil {
			return
		}
	}
}

// wait blocks until a webhook notification, the poll interval, or shutdown.
// It reports false when the worker should stop.
func (w *Worker) wait(ctx context.Context) bool {
	timer := time.NewTimer(w.cfg.PollInterval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-w.notify:
		return true
	case <-timer.C:
		return true
	}
}

// runJob executes one job and records its outcome. Failures are retried until
// MaxAttempts, then dead-lettered with a note on the PR so a silent drop is
// impossible.
func (w *Worker) runJob(ctx context.Context, job store.Job) {
	log := w.log.With("job", job.ID, "pr", job.PRKey(), "head", job.HeadSHA, "attempt", job.Attempts)
	log.Info("job started")

	start := time.Now()

	err := w.review(ctx, job, log)
	if err == nil {
		if ferr := w.store.Finish(ctx, job.ID, store.StateDone, nil); ferr != nil {
			log.Error("recording job success failed", "error", ferr)
		}

		log.Info("job done", "duration", time.Since(start).String())

		return
	}

	if job.Attempts < w.cfg.MaxAttempts {
		if ferr := w.store.Finish(ctx, job.ID, store.StateFailed, err); ferr != nil {
			log.Error("recording job failure failed", "error", ferr)
		}

		log.Warn("job failed, will retry", "error", err, "retry_in", w.cfg.RetryDelay.String())
		w.sleep(ctx, w.cfg.RetryDelay)

		return
	}

	if ferr := w.store.Finish(ctx, job.ID, store.StateDead, err); ferr != nil {
		log.Error("recording dead job failed", "error", ferr)
	}

	log.Error("job dead-lettered", "error", err)
	w.reportFailure(ctx, job, err, log)
}

func (w *Worker) sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// review is the whole review pass for one job.
func (w *Worker) review(ctx context.Context, job store.Job, log *slog.Logger) error {
	prov, ok := w.providers[job.Provider]
	if !ok {
		// Not retryable: no credentials will appear by themselves.
		log.Error("no provider configured", "provider", job.Provider)

		return fmt.Errorf("provider %q not configured", job.Provider)
	}

	prKey := job.PRKey()

	if job.Event == store.EventReopened && job.Attempts == 1 {
		if err := w.store.ResetPRState(ctx, prKey); err != nil {
			return err
		}
	}

	state, err := w.store.PRState(ctx, prKey)
	if err != nil {
		return err
	}

	if state.LastReviewedSHA == job.HeadSHA {
		log.Info("head already reviewed, skipping")

		return nil
	}

	pr, err := prov.PullRequest(ctx, job.Repo, job.PRNumber)
	if err != nil {
		return err
	}

	// Answering the author about findings already reported comes first, and
	// happens whether or not a review cycle is left: once the budget is spent
	// the follow-up is the only thing the worker still owes the pull request.
	verified, err := w.verify(ctx, job, prov, state, pr, log)
	if err != nil {
		return err
	}

	if state.Cycle >= w.cfg.MaxCycles {
		approved, err := w.approve(ctx, job, prov, state, verified, 0, log)
		if err != nil {
			return err
		}

		// The notice goes first. Recording the head before it is posted would
		// make the retry of a failed notice exit early as "head already
		// reviewed", so the author would never be told the budget is spent.
		if err := w.handleExhausted(ctx, job, prov, &state, log); err != nil {
			return err
		}

		if !verified.Ran {
			return nil
		}

		// The follow-up is this pass's whole contribution; recording the head
		// keeps the next push from re-verifying the same threads.
		state.LastReviewedSHA = job.HeadSHA
		state.Approved = state.Approved || approved

		return w.store.SavePRState(ctx, prKey, state)
	}

	cycle := state.Cycle + 1

	diff, err := w.scopedDiff(ctx, job, prov, state, cycle)
	if err != nil {
		return err
	}

	if strings.TrimSpace(diff) == "" {
		// Nothing to review (e.g. a force-push that reverted to the reviewed
		// tree). Spending a cycle on an empty diff would waste the budget.
		log.Info("empty diff, no cycle consumed")

		return nil
	}

	res, err := w.engine.Review(ctx, reviewer.Request{
		Repo:     job.Repo,
		PRNumber: job.PRNumber,
		Title:    pr.Title,
		Body:     pr.Body,
		Diff:     diff,
		Cycle:    cycle,
	})
	if err != nil {
		return err
	}

	log.Info("engine finished", "engine", res.Engine, "findings", len(res.Findings))

	posted, unposted, err := w.postFindings(ctx, job, prov, res, log)
	if err != nil {
		return err
	}

	body := renderSummary(res, cycle, w.cfg.MaxCycles, posted, unposted)

	commentID, err := w.postSummary(ctx, job, prov, state, cycle, body)
	if err != nil {
		return err
	}

	state.SummaryCommentID = commentID
	state.SummaryCycle = cycle

	if err := w.store.SavePRState(ctx, prKey, state); err != nil {
		return err
	}

	state.Cycle = cycle
	state.LastReviewedSHA = job.HeadSHA

	approved, err := w.approve(ctx, job, prov, state, verified, len(posted)+len(unposted), log)
	if err != nil {
		return err
	}

	state.Approved = state.Approved || approved

	if err := w.store.SavePRState(ctx, prKey, state); err != nil {
		return err
	}

	return nil
}

// scopedDiff picks the diff for this pass: the whole PR on the first cycle,
// only what changed since the reviewed SHA on the second. Re-reviewing the
// full diff would re-derive findings already posted and burn engine quota on
// unchanged code.
func (w *Worker) scopedDiff(
	ctx context.Context,
	job store.Job,
	prov provider.Provider,
	state store.PRState,
	cycle int,
) (string, error) {
	if cycle == 1 || state.LastReviewedSHA == "" {
		return prov.Diff(ctx, job.Repo, job.PRNumber)
	}

	diff, err := prov.CompareDiff(ctx, job.Repo, state.LastReviewedSHA, job.HeadSHA)
	if err == nil {
		return diff, nil
	}

	// A force-push can orphan the previously reviewed SHA, making the
	// comparison unresolvable; the full diff is the only correct fallback.
	w.log.Warn(
		"incremental diff unavailable, falling back to full diff",
		"pr", job.PRKey(),
		"from", state.LastReviewedSHA,
		"error", err,
	)

	return prov.Diff(ctx, job.Repo, job.PRNumber)
}

// postFindings posts the inline comments that survive severity filtering,
// cross-cycle fingerprint dedup, and the per-cycle cap — in that order, so
// findings already commented on in an earlier pass never consume the budget.
// It returns the findings actually posted and the ones that were not, so the
// summary can list both and no finding is silently dropped.
func (w *Worker) postFindings(
	ctx context.Context,
	job store.Job,
	prov provider.Provider,
	res reviewer.Result,
	log *slog.Logger,
) ([]reviewer.Finding, []reviewer.Finding, error) {
	byFingerprint := make(map[string]reviewer.Finding, len(res.Findings))
	fingerprints := make([]string, 0, len(res.Findings))

	for _, f := range res.Findings {
		if !f.Severity.AtLeast(w.cfg.MinSeverity) {
			continue
		}

		fp := f.Fingerprint()
		if _, seen := byFingerprint[fp]; seen {
			continue
		}

		byFingerprint[fp] = f
		fingerprints = append(fingerprints, fp)
	}

	fresh, err := w.store.UnseenFingerprints(ctx, job.PRKey(), fingerprints)
	if err != nil {
		return nil, nil, err
	}

	sort.SliceStable(fresh, func(i, j int) bool {
		return byFingerprint[fresh[i]].Severity.Rank() > byFingerprint[fresh[j]].Severity.Rank()
	})

	var overCap []string

	if w.cfg.MaxComments > 0 && len(fresh) > w.cfg.MaxComments {
		fresh, overCap = fresh[:w.cfg.MaxComments], fresh[w.cfg.MaxComments:]
	}

	posted := make([]reviewer.Finding, 0, len(fresh))
	unposted := make([]reviewer.Finding, 0, len(overCap))

	for _, fp := range fresh {
		f := byFingerprint[fp]

		err := prov.PostInline(ctx, job.Repo, job.PRNumber, job.HeadSHA, provider.InlineComment{
			Path: f.File,
			Line: f.Line,
			Body: renderComment(f),
		})
		if err != nil {
			// A line outside the diff is rejected by the forge. That is a bad
			// citation from the engine, not a worker failure: the finding
			// still reaches the reviewer through the summary. The fingerprint
			// stays unrecorded, so a transient failure is retried next pass
			// rather than suppressing the finding forever.
			log.Warn("inline comment rejected", "file", f.File, "line", f.Line, "error", err)

			unposted = append(unposted, f)

			continue
		}

		if err := w.store.RecordFingerprint(ctx, job.PRKey(), fp); err != nil {
			return nil, nil, err
		}

		posted = append(posted, f)
	}

	for _, fp := range overCap {
		unposted = append(unposted, byFingerprint[fp])
	}

	log.Info(
		"comments posted",
		"candidates", len(fingerprints),
		"fresh", len(fresh),
		"posted", len(posted),
		"unposted", len(unposted),
	)

	return posted, unposted, nil
}

// postSummary posts a new summary comment, or edits the existing one when this
// is a retry of the same cycle, so a retried job never leaves two summaries.
func (w *Worker) postSummary(
	ctx context.Context,
	job store.Job,
	prov provider.Provider,
	state store.PRState,
	cycle int,
	body string,
) (string, error) {
	if state.SummaryCommentID != "" && state.SummaryCycle == cycle {
		if err := prov.UpdateSummary(ctx, job.Repo, state.SummaryCommentID, body); err != nil {
			return "", err
		}

		return state.SummaryCommentID, nil
	}

	return prov.PostSummary(ctx, job.Repo, job.PRNumber, body)
}

// handleExhausted deals with a push after the review budget is spent: one note,
// once, then silence. Repeating it on every push would be noise, and saying
// nothing at all leaves the author waiting for a review that never comes.
// handleExhausted posts the one budget notice a pull request gets, if it has
// not had it yet.
//
// state is a pointer because the caller may save it again afterwards: taking a
// copy here meant `BudgetNoticePosted = true` was written to the database and
// then immediately overwritten with the caller's stale `false`, so the notice
// reposted on every later push instead of once.
func (w *Worker) handleExhausted(
	ctx context.Context,
	job store.Job,
	prov provider.Provider,
	state *store.PRState,
	log *slog.Logger,
) error {
	if !w.cfg.AnnounceBudgetExhausted || state.BudgetNoticePosted {
		log.Info("review budget exhausted, staying silent", "cycles", state.Cycle)

		return nil
	}

	body := fmt.Sprintf(
		"%s\n\n**Review budget exhausted** — this pull request already had its %d automated review passes. "+
			"Further pushes will not be reviewed. Re-open the pull request to reset the budget.\n",
		summaryMarker,
		w.cfg.MaxCycles,
	)

	if _, err := prov.PostSummary(ctx, job.Repo, job.PRNumber, body); err != nil {
		return err
	}

	state.BudgetNoticePosted = true

	return w.store.SavePRState(ctx, job.PRKey(), *state)
}

// reportFailure tells the PR author that the review will not arrive. A silent
// dead-letter is indistinguishable from "the code is fine".
func (w *Worker) reportFailure(ctx context.Context, job store.Job, cause error, log *slog.Logger) {
	prov, ok := w.providers[job.Provider]
	if !ok {
		return
	}

	// The full cause (command output, paths, response bodies) stays in the
	// log only: it can carry internal details that should not reach a
	// public PR comment.
	body := fmt.Sprintf(
		"%s\n\n**Automated review failed** after %d attempts and will not be retried for `%s`. "+
			"See the worker log for details.\n",
		summaryMarker,
		job.Attempts,
		job.HeadSHA,
	)

	if _, err := prov.PostSummary(ctx, job.Repo, job.PRNumber, body); err != nil {
		log.Error("posting failure note failed", "error", err)
	}
}
