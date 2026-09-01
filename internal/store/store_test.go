package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dhiazfathra/go-pr-review-worker/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st
}

func job(delivery string, number int) store.Job {
	return store.Job{
		DeliveryID: delivery,
		Provider:   "github",
		Repo:       "acme/app",
		PRNumber:   number,
		HeadSHA:    "sha-" + delivery,
		BaseSHA:    "base",
		Event:      store.EventOpened,
	}
}

func TestOpenFailsOnUnwritablePath(t *testing.T) {
	if _, err := store.Open(filepath.Join(t.TempDir(), "missing", "dir", "x.db")); err == nil {
		t.Fatal("want error for unwritable path, got nil")
	}
}

func TestEnqueueIsIdempotentPerDelivery(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	first, err := st.Enqueue(ctx, job("d1", 1))
	if err != nil || !first {
		t.Fatalf("first Enqueue = %v, %v; want true, nil", first, err)
	}

	second, err := st.Enqueue(ctx, job("d1", 1))
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}

	if second {
		t.Fatal("redelivery enqueued a second job; cycle budget would be double-spent")
	}

	pending, err := st.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}

	if pending != 1 {
		t.Fatalf("PendingCount = %d, want 1", pending)
	}
}

func TestClaimNextIsFIFOAcrossRepos(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	for _, d := range []string{"a", "b", "c"} {
		if _, err := st.Enqueue(ctx, job(d, 1)); err != nil {
			t.Fatalf("Enqueue %s: %v", d, err)
		}
	}

	for _, want := range []string{"a", "b", "c"} {
		got, err := st.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("ClaimNext: %v", err)
		}

		if got.DeliveryID != want {
			t.Fatalf("ClaimNext = %q, want %q", got.DeliveryID, want)
		}

		if got.Attempts != 1 {
			t.Fatalf("Attempts = %d, want 1", got.Attempts)
		}

		if err := st.Finish(ctx, got.ID, store.StateDone, nil); err != nil {
			t.Fatalf("Finish: %v", err)
		}
	}

	if _, err := st.ClaimNext(ctx); !errors.Is(err, store.ErrNoJob) {
		t.Fatalf("ClaimNext on empty queue = %v, want ErrNoJob", err)
	}
}

func TestFailedJobIsRetriedAndCountsAttempts(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	if _, err := st.Enqueue(ctx, job("d1", 7)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	first, err := st.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if err := st.Finish(ctx, first.ID, store.StateFailed, errors.New("boom")); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	again, err := st.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("second ClaimNext: %v", err)
	}

	if again.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", again.Attempts)
	}

	if err := st.Finish(ctx, again.ID, store.StateDead, errors.New("boom")); err != nil {
		t.Fatalf("Finish dead: %v", err)
	}

	if _, err := st.ClaimNext(ctx); !errors.Is(err, store.ErrNoJob) {
		t.Fatalf("dead job is still runnable: %v", err)
	}
}

func TestPRStateRoundTrip(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	zero, err := st.PRState(ctx, "github:acme/app#1")
	if err != nil {
		t.Fatalf("PRState: %v", err)
	}

	if zero != (store.PRState{}) {
		t.Fatalf("unknown PR state = %+v, want zero", zero)
	}

	want := store.PRState{
		Cycle:              2,
		LastReviewedSHA:    "abc",
		SummaryCommentID:   "42",
		SummaryCycle:       2,
		BudgetNoticePosted: true,
	}

	if err := st.SavePRState(ctx, "github:acme/app#1", want); err != nil {
		t.Fatalf("SavePRState: %v", err)
	}

	// Overwriting the same key must update, not duplicate.
	want.Cycle = 2

	if err := st.SavePRState(ctx, "github:acme/app#1", want); err != nil {
		t.Fatalf("SavePRState update: %v", err)
	}

	got, err := st.PRState(ctx, "github:acme/app#1")
	if err != nil {
		t.Fatalf("PRState: %v", err)
	}

	if got != want {
		t.Fatalf("PRState = %+v, want %+v", got, want)
	}
}

func TestUnseenFingerprintsOnlyHidesRecordedOnes(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	first, err := st.UnseenFingerprints(ctx, "pr", []string{"a", "b"})
	if err != nil {
		t.Fatalf("UnseenFingerprints: %v", err)
	}

	if len(first) != 2 {
		t.Fatalf("first read = %v, want 2 entries", first)
	}

	// Reading must not record: an unposted comment has to stay eligible, or a
	// failed post would suppress the finding forever.
	again, err := st.UnseenFingerprints(ctx, "pr", []string{"a", "b"})
	if err != nil {
		t.Fatalf("UnseenFingerprints: %v", err)
	}

	if len(again) != 2 {
		t.Fatalf("second read = %v; reading marked them posted", again)
	}

	if err := st.RecordFingerprint(ctx, "pr", "a"); err != nil {
		t.Fatalf("RecordFingerprint: %v", err)
	}

	// Recording twice is harmless.
	if err := st.RecordFingerprint(ctx, "pr", "a"); err != nil {
		t.Fatalf("RecordFingerprint repeat: %v", err)
	}

	after, err := st.UnseenFingerprints(ctx, "pr", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("UnseenFingerprints: %v", err)
	}

	if len(after) != 2 || after[0] != "b" || after[1] != "c" {
		t.Fatalf("after recording = %v, want [b c]", after)
	}

	// A different PR must not inherit the first PR's dedup state.
	other, err := st.UnseenFingerprints(ctx, "other", []string{"a"})
	if err != nil {
		t.Fatalf("UnseenFingerprints: %v", err)
	}

	if len(other) != 1 {
		t.Fatalf("other PR = %v, want [a]", other)
	}
}

func TestClosedStoreReportsErrors(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := st.Enqueue(ctx, job("d", 1)); err == nil {
		t.Fatal("Enqueue on closed store: want error")
	}

	if _, err := st.ClaimNext(ctx); err == nil {
		t.Fatal("ClaimNext on closed store: want error")
	}

	if _, err := st.PRState(ctx, "pr"); err == nil {
		t.Fatal("PRState on closed store: want error")
	}

	if err := st.SavePRState(ctx, "pr", store.PRState{}); err == nil {
		t.Fatal("SavePRState on closed store: want error")
	}

	if _, err := st.UnseenFingerprints(ctx, "pr", []string{"a"}); err == nil {
		t.Fatal("UnseenFingerprints on closed store: want error")
	}

	if err := st.RecordFingerprint(ctx, "pr", "a"); err == nil {
		t.Fatal("RecordFingerprint on closed store: want error")
	}

	if err := st.Finish(ctx, 1, store.StateDone, errors.New("x")); err == nil {
		t.Fatal("Finish on closed store: want error")
	}

	if _, err := st.PendingCount(ctx); err == nil {
		t.Fatal("PendingCount on closed store: want error")
	}
}

func TestPRKey(t *testing.T) {
	got := store.Job{Provider: "gitlab", Repo: "grp/proj", PRNumber: 9}.PRKey()
	if got != "gitlab:grp/proj#9" {
		t.Fatalf("PRKey = %q", got)
	}
}

func TestUnseenFingerprintsHandlesEmptyInput(t *testing.T) {
	st := open(t)

	got, err := st.UnseenFingerprints(t.Context(), "pr", nil)
	if err != nil {
		t.Fatalf("UnseenFingerprints: %v", err)
	}

	if len(got) != 0 {
		t.Fatalf("got = %v, want empty", got)
	}
}

func TestUnseenFingerprintsPreservesInputOrder(t *testing.T) {
	// Review feedback (PR #1, pass 2): the batched query returns rows in the
	// database's order, so the result has to be rebuilt from the input.
	st := open(t)
	ctx := t.Context()

	if err := st.RecordFingerprint(ctx, "pr", "b"); err != nil {
		t.Fatalf("RecordFingerprint: %v", err)
	}

	got, err := st.UnseenFingerprints(ctx, "pr", []string{"c", "b", "a"})
	if err != nil {
		t.Fatalf("UnseenFingerprints: %v", err)
	}

	if len(got) != 2 || got[0] != "c" || got[1] != "a" {
		t.Fatalf("got = %v, want [c a]", got)
	}
}
