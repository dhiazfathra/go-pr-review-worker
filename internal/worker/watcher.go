package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dhiazfathra/go-pr-review-worker/internal/provider"
	"github.com/dhiazfathra/go-pr-review-worker/internal/store"
)

// WatchTarget is one repository the watcher polls.
type WatchTarget struct {
	Provider string
	Repo     string
}

// Watcher enqueues review jobs for pushes no webhook delivered.
//
// A webhook is the fast path, not a reliable one: a delivery that arrives while
// the worker is down is gone, a repository can simply have no hook configured,
// and a hook can be removed without anyone noticing until a pull request sits
// unreviewed. The queue's idempotency key is the head SHA, so polling for the
// same push a webhook already delivered collapses into the existing job rather
// than reviewing twice — which makes the two mechanisms safe to run together.
type Watcher struct {
	store     *store.Store
	providers map[string]provider.Provider
	targets   []WatchTarget
	notify    chan<- struct{}
	interval  time.Duration
	log       *slog.Logger
}

// NewWatcher builds a watcher. An empty targets list yields a watcher whose Run
// returns immediately, so the caller need not special-case "watching nothing".
func NewWatcher(
	st *store.Store,
	providers map[string]provider.Provider,
	targets []WatchTarget,
	notify chan<- struct{},
	interval time.Duration,
	log *slog.Logger,
) *Watcher {
	if interval <= 0 {
		interval = 2 * time.Minute
	}

	return &Watcher{
		store:     st,
		providers: providers,
		targets:   targets,
		notify:    notify,
		interval:  interval,
		log:       log,
	}
}

// Run polls until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	if len(w.targets) == 0 {
		return
	}

	w.log.Info("watching repositories", "count", len(w.targets), "interval", w.interval.String())

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		if err := w.Sweep(ctx); err != nil {
			w.log.Warn("watch sweep failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep polls every watched repository once and enqueues what is missing. One
// repository's failure never stops the others — a revoked token on one project
// must not stop reviews everywhere else — so the first error is returned only
// after every target has had its turn.
func (w *Watcher) Sweep(ctx context.Context) error {
	var firstErr error

	for _, t := range w.targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err := w.sweep(ctx, t); err != nil {
			w.log.Warn("watching repository failed", "provider", t.Provider, "repo", t.Repo, "error", err)

			if firstErr == nil {
				firstErr = fmt.Errorf("%s:%s: %w", t.Provider, t.Repo, err)
			}
		}
	}

	return firstErr
}

// sweep enqueues a job for every open pull request whose head has moved past
// what was last reviewed.
func (w *Watcher) sweep(ctx context.Context, t WatchTarget) error {
	prov, ok := w.providers[t.Provider]
	if !ok {
		return fmt.Errorf("provider %q not configured", t.Provider)
	}

	prs, err := prov.ListOpenPullRequests(ctx, t.Repo)
	if err != nil {
		return err
	}

	for _, pr := range prs {
		if pr.HeadSHA == "" {
			continue
		}

		job := store.Job{
			// Same key shape the webhook path builds, so a push seen by both
			// mechanisms is one job, not two.
			DeliveryID: fmt.Sprintf("%s:%s#%d:%s", t.Provider, t.Repo, pr.Number, pr.HeadSHA),
			Provider:   t.Provider,
			Repo:       t.Repo,
			PRNumber:   pr.Number,
			HeadSHA:    pr.HeadSHA,
			BaseSHA:    pr.BaseSHA,
			Event:      store.EventSynchronize,
		}

		state, err := w.store.PRState(ctx, job.PRKey())
		if err != nil {
			return err
		}

		// Nothing new since the last completed review: the worker would skip
		// this job anyway, so it is not worth a queue row.
		if state.LastReviewedSHA == pr.HeadSHA {
			continue
		}

		// A pull request the worker has never seen is an "opened" it missed,
		// not a push: the first pass must review the whole diff.
		if state.Cycle == 0 && state.LastReviewedSHA == "" {
			job.Event = store.EventOpened
		}

		enqueued, err := w.store.Enqueue(ctx, job)
		if err != nil {
			return err
		}

		if !enqueued {
			continue
		}

		w.log.Info(
			"job enqueued by watcher",
			"delivery", job.DeliveryID,
			"pr", job.PRKey(),
			"event", string(job.Event),
			"head", job.HeadSHA,
		)

		select {
		case w.notify <- struct{}{}:
		default: // the worker is busy or already has a pending wake-up
		}
	}

	return nil
}
