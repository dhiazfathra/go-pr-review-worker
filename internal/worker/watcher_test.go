package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/dhiazfathra/go-pr-review-worker/internal/provider"
	"github.com/dhiazfathra/go-pr-review-worker/internal/store"
	"github.com/dhiazfathra/go-pr-review-worker/internal/worker"
)

func newWatcherFixture(t *testing.T, prov *fakeProvider) (*store.Store, *worker.Watcher, chan struct{}) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "watch.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	notify := make(chan struct{}, 1)

	w := worker.NewWatcher(
		st,
		map[string]provider.Provider{"github": prov},
		[]worker.WatchTarget{{Provider: "github", Repo: "o/r"}},
		notify,
		time.Hour, // one sweep per Run; the test cancels before the second tick
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	return st, w, notify
}

func TestWatcherEnqueuesAPushNoWebhookDelivered(t *testing.T) {
	prov := &fakeProvider{
		openPRs: []provider.OpenPullRequest{{Number: 7, HeadSHA: "sha2", BaseSHA: "base"}},
	}

	st, w, notify := newWatcherFixture(t, prov)

	// Pass 1 already happened at sha1; the push to sha2 never arrived as a
	// webhook, which is exactly the gap the watcher closes.
	if err := st.SavePRState(context.Background(), "github:o/r#7", store.PRState{
		Cycle:           1,
		LastReviewedSHA: "sha1",
	}); err != nil {
		t.Fatalf("SavePRState: %v", err)
	}

	if err := w.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	job, err := st.ClaimNext(context.Background())
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if job.HeadSHA != "sha2" || job.PRNumber != 7 {
		t.Fatalf("job = %+v, want the sha2 push on PR 7", job)
	}

	if job.Event != store.EventSynchronize {
		t.Errorf("event = %q, want synchronize for a PR already reviewed once", job.Event)
	}

	select {
	case <-notify:
	default:
		t.Error("the worker was not woken after the watcher enqueued a job")
	}
}

func TestWatcherTreatsAnUnseenPullRequestAsOpened(t *testing.T) {
	prov := &fakeProvider{
		openPRs: []provider.OpenPullRequest{{Number: 3, HeadSHA: "sha1"}},
	}

	st, w, _ := newWatcherFixture(t, prov)

	if err := w.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	job, err := st.ClaimNext(context.Background())
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if job.Event != store.EventOpened {
		t.Fatalf("event = %q, want opened: a never-reviewed PR needs the full diff", job.Event)
	}
}

func TestWatcherSkipsAHeadAlreadyReviewed(t *testing.T) {
	prov := &fakeProvider{
		openPRs: []provider.OpenPullRequest{{Number: 7, HeadSHA: "sha1"}},
	}

	st, w, _ := newWatcherFixture(t, prov)

	if err := st.SavePRState(context.Background(), "github:o/r#7", store.PRState{
		Cycle:           1,
		LastReviewedSHA: "sha1",
	}); err != nil {
		t.Fatalf("SavePRState: %v", err)
	}

	if err := w.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, err := st.ClaimNext(context.Background()); !errors.Is(err, store.ErrNoJob) {
		t.Fatalf("ClaimNext err = %v, want ErrNoJob: the reviewed head must not requeue", err)
	}
}

// The webhook path and the watcher can both see the same push. They must
// collapse to one job, or a PR would be reviewed twice for one commit.
func TestWatcherDoesNotDuplicateAWebhookDelivery(t *testing.T) {
	prov := &fakeProvider{
		openPRs: []provider.OpenPullRequest{{Number: 7, HeadSHA: "sha2"}},
	}

	st, w, _ := newWatcherFixture(t, prov)

	webhookJob := store.Job{
		DeliveryID: "github:o/r#7:sha2",
		Provider:   "github",
		Repo:       "o/r",
		PRNumber:   7,
		HeadSHA:    "sha2",
		Event:      store.EventSynchronize,
	}

	if _, err := st.Enqueue(context.Background(), webhookJob); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := w.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	first, err := st.ClaimNext(context.Background())
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	// ClaimNext re-serves a job left running, so "no second job" cannot be
	// asserted by a failing claim — the duplicate would have to be a different
	// row. Same id means the watcher collapsed into the webhook's job.
	second, err := st.ClaimNext(context.Background())
	if err != nil {
		t.Fatalf("second ClaimNext: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf(
			"claimed job ids %d then %d: the watcher enqueued a duplicate of the webhook delivery",
			first.ID, second.ID,
		)
	}
}

func TestWatcherKeepsGoingWhenOneRepositoryFails(t *testing.T) {
	prov := &fakeProvider{openPRErr: errors.New("token revoked")}

	_, w, _ := newWatcherFixture(t, prov)

	if err := w.Sweep(context.Background()); err == nil {
		t.Fatal("Sweep err = nil, want the provider failure surfaced to the caller")
	}
}

func TestWatcherWithNoTargetsStopsImmediately(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "idle.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	w := worker.NewWatcher(
		st,
		map[string]provider.Provider{},
		nil,
		make(chan struct{}, 1),
		time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	done := make(chan struct{})

	go func() {
		defer close(done)
		w.Run(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with nothing to watch")
	}
}
