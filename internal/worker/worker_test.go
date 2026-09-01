package worker_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhiazfathra/go-pr-review-worker/internal/provider"
	"github.com/dhiazfathra/go-pr-review-worker/internal/reviewer"
	"github.com/dhiazfathra/go-pr-review-worker/internal/store"
	"github.com/dhiazfathra/go-pr-review-worker/internal/worker"
)

// fakeProvider records everything posted so a test can assert on the review
// the author would actually see.
type fakeProvider struct {
	mu sync.Mutex

	pr          provider.PullRequest
	fullDiff    string
	compareDiff string
	compareErr  error
	inlineErr   error

	inline         []provider.InlineComment
	summaries      []string
	updates        []string
	compareCalls   [][2]string
	fullDiffCalls  int
	postSummaryErr error
}

func (f *fakeProvider) Name() string { return "github" }

func (f *fakeProvider) PullRequest(context.Context, string, int) (provider.PullRequest, error) {
	return f.pr, nil
}

func (f *fakeProvider) Diff(context.Context, string, int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.fullDiffCalls++

	return f.fullDiff, nil
}

func (f *fakeProvider) CompareDiff(_ context.Context, _, from, to string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.compareCalls = append(f.compareCalls, [2]string{from, to})

	if f.compareErr != nil {
		return "", f.compareErr
	}

	return f.compareDiff, nil
}

func (f *fakeProvider) PostInline(
	_ context.Context,
	_ string,
	_ int,
	_ string,
	c provider.InlineComment,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.inlineErr != nil {
		return f.inlineErr
	}

	f.inline = append(f.inline, c)

	return nil
}

func (f *fakeProvider) PostSummary(_ context.Context, _ string, _ int, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.postSummaryErr != nil {
		return "", f.postSummaryErr
	}

	f.summaries = append(f.summaries, body)

	return fmt.Sprintf("summary-%d", len(f.summaries)), nil
}

func (f *fakeProvider) UpdateSummary(_ context.Context, _, _, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.updates = append(f.updates, body)

	return nil
}

// scriptedEngine returns a queued result per call, so a two-cycle test can give
// each pass its own findings.
type scriptedEngine struct {
	mu      sync.Mutex
	results []reviewer.Result
	errs    []error
	calls   int
	prompts []reviewer.Request
}

func (s *scriptedEngine) Name() string { return "scripted" }

// callCount reads the invocation count under the lock; the worker runs on its
// own goroutine, so a bare field read would race.
func (s *scriptedEngine) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

func (s *scriptedEngine) request(i int) reviewer.Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.prompts[i]
}

func (s *scriptedEngine) Review(_ context.Context, req reviewer.Request) (reviewer.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.calls
	s.calls++
	s.prompts = append(s.prompts, req)

	if i < len(s.errs) && s.errs[i] != nil {
		return reviewer.Result{}, s.errs[i]
	}

	if i >= len(s.results) {
		return reviewer.Result{}, errors.New("engine called more times than scripted")
	}

	res := s.results[i]
	res.Engine = "scripted"

	return res, nil
}

func finding(file, title string, sev reviewer.Severity) reviewer.Finding {
	return reviewer.Finding{File: file, Line: 3, Severity: sev, Title: title, Body: "why"}
}

type harness struct {
	st     *store.Store
	prov   *fakeProvider
	engine *scriptedEngine
	worker *worker.Worker
	notify chan struct{}
}

func newHarness(t *testing.T, cfg worker.Config, engine *scriptedEngine) *harness {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	prov := &fakeProvider{
		pr:          provider.PullRequest{Title: "Add cache", Body: "desc", HeadSHA: "sha1"},
		fullDiff:    "@@ full diff @@",
		compareDiff: "@@ delta diff @@",
	}

	notify := make(chan struct{}, 1)

	w := worker.New(
		st,
		engine,
		map[string]provider.Provider{"github": prov},
		notify,
		cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	return &harness{st: st, prov: prov, engine: engine, worker: w, notify: notify}
}

// runOnce drains the queue and returns once it is empty, without racing the
// long-lived Run loop.
func (h *harness) runOnce(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	cfgDone := make(chan struct{})

	go func() {
		defer close(cfgDone)
		h.worker.Run(ctx)
	}()

	deadline := time.Now().Add(10 * time.Second)

	for {
		n, err := h.st.PendingCount(context.Background())
		if err != nil {
			t.Fatalf("PendingCount: %v", err)
		}

		if n == 0 {
			break
		}

		if time.Now().After(deadline) {
			cancel()
			t.Fatal("queue did not drain")
		}

		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-cfgDone
}

func (h *harness) enqueue(t *testing.T, delivery, head string, event store.Event) {
	t.Helper()

	ok, err := h.st.Enqueue(context.Background(), store.Job{
		DeliveryID: delivery,
		Provider:   "github",
		Repo:       "acme/app",
		PRNumber:   7,
		HeadSHA:    head,
		Event:      event,
	})
	if err != nil || !ok {
		t.Fatalf("Enqueue = %v, %v", ok, err)
	}
}

func TestFirstCyclePostsInlineCommentsAndSummary(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{{
		Summary: "one real problem",
		Findings: []reviewer.Finding{
			finding("a.go", "nil deref", reviewer.SeverityCritical),
			finding("b.go", "style thing", reviewer.SeverityNit), // below threshold
		},
	}}}

	h := newHarness(t, worker.Config{MinSeverity: reviewer.SeverityMinor}, engine)
	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	if len(h.prov.inline) != 1 || h.prov.inline[0].Path != "a.go" {
		t.Fatalf("inline = %+v, want only the critical finding", h.prov.inline)
	}

	if len(h.prov.summaries) != 1 {
		t.Fatalf("summaries = %d, want 1", len(h.prov.summaries))
	}

	if !strings.Contains(h.prov.summaries[0], "pass 1 of 2") {
		t.Fatalf("summary does not label the pass:\n%s", h.prov.summaries[0])
	}

	if h.prov.fullDiffCalls != 1 {
		t.Fatalf("full diff fetched %d times, want 1", h.prov.fullDiffCalls)
	}

	state, err := h.st.PRState(context.Background(), "github:acme/app#7")
	if err != nil {
		t.Fatalf("PRState: %v", err)
	}

	if state.Cycle != 1 || state.LastReviewedSHA != "sha1" {
		t.Fatalf("state = %+v, want cycle 1 at sha1", state)
	}
}

func TestSecondCycleUsesIncrementalDiffAndDedupesFindings(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{
		{Summary: "pass 1", Findings: []reviewer.Finding{finding("a.go", "nil deref", reviewer.SeverityMajor)}},
		{Summary: "pass 2", Findings: []reviewer.Finding{
			finding("a.go", "nil deref", reviewer.SeverityMajor), // already posted
			finding("c.go", "new leak", reviewer.SeverityMajor),
		}},
	}}

	h := newHarness(t, worker.Config{}, engine)

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	h.prov.pr.HeadSHA = "sha2"
	h.enqueue(t, "d2", "sha2", store.EventSynchronize)
	h.runOnce(t)

	if len(h.prov.compareCalls) != 1 || h.prov.compareCalls[0] != [2]string{"sha1", "sha2"} {
		t.Fatalf("compare calls = %v, want one sha1..sha2", h.prov.compareCalls)
	}

	if h.engine.request(1).Diff != "@@ delta diff @@" {
		t.Fatalf("second pass reviewed %q, want the delta", h.engine.request(1).Diff)
	}

	if h.engine.request(1).Cycle != 2 {
		t.Fatalf("second pass cycle = %d", h.engine.request(1).Cycle)
	}

	paths := []string{}
	for _, c := range h.prov.inline {
		paths = append(paths, c.Path)
	}

	if strings.Join(paths, ",") != "a.go,c.go" {
		t.Fatalf("inline paths = %v, want a.go once and c.go once", paths)
	}

	if len(h.prov.summaries) != 2 {
		t.Fatalf("summaries = %d, want one per cycle", len(h.prov.summaries))
	}

	if !strings.Contains(h.prov.summaries[1], "Final pass") {
		t.Fatalf("last summary does not announce the budget:\n%s", h.prov.summaries[1])
	}
}

func TestThirdPushPostsOneBudgetNoticeThenStaysSilent(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{
		{Summary: "pass 1"},
		{Summary: "pass 2"},
	}}

	h := newHarness(t, worker.Config{AnnounceBudgetExhausted: true}, engine)

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	h.prov.pr.HeadSHA = "sha2"
	h.enqueue(t, "d2", "sha2", store.EventSynchronize)
	h.runOnce(t)

	h.prov.pr.HeadSHA = "sha3"
	h.enqueue(t, "d3", "sha3", store.EventSynchronize)
	h.runOnce(t)

	h.enqueue(t, "d4", "sha4", store.EventSynchronize)
	h.runOnce(t)

	if h.engine.callCount() != 2 {
		t.Fatalf("engine ran %d times, want exactly 2 review cycles", h.engine.callCount())
	}

	if len(h.prov.summaries) != 3 {
		t.Fatalf("summaries = %d, want 2 reviews + 1 budget notice", len(h.prov.summaries))
	}

	if !strings.Contains(h.prov.summaries[2], "Review budget exhausted") {
		t.Fatalf("third comment is not the budget notice:\n%s", h.prov.summaries[2])
	}
}

func TestBudgetNoticeCanBeSuppressed(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{{Summary: "p1"}, {Summary: "p2"}}}
	h := newHarness(t, worker.Config{AnnounceBudgetExhausted: false}, engine)

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	h.prov.pr.HeadSHA = "sha2"
	h.enqueue(t, "d2", "sha2", store.EventSynchronize)
	h.runOnce(t)

	h.enqueue(t, "d3", "sha3", store.EventSynchronize)
	h.runOnce(t)

	if len(h.prov.summaries) != 2 {
		t.Fatalf("summaries = %d, want no budget notice", len(h.prov.summaries))
	}
}

func TestCycleBudgetSurvivesWorkerRestart(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{{Summary: "p1"}, {Summary: "p2"}}}
	h := newHarness(t, worker.Config{}, engine)

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	// A restart replaces the in-process worker but keeps the same database.
	restarted := worker.New(
		h.st,
		engine,
		map[string]provider.Provider{"github": h.prov},
		h.notify,
		worker.Config{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	h.worker = restarted

	h.prov.pr.HeadSHA = "sha2"
	h.enqueue(t, "d2", "sha2", store.EventSynchronize)
	h.runOnce(t)

	h.enqueue(t, "d3", "sha3", store.EventSynchronize)
	h.runOnce(t)

	if engine.callCount() != 2 {
		t.Fatalf("engine ran %d times after restart, want 2: the budget was not persisted", engine.callCount())
	}
}

func TestEmptyDeltaDoesNotSpendACycle(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{{Summary: "p1"}, {Summary: "p2"}}}
	h := newHarness(t, worker.Config{}, engine)

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	h.prov.compareDiff = "   \n"
	h.prov.pr.HeadSHA = "sha2"
	h.enqueue(t, "d2", "sha2", store.EventSynchronize)
	h.runOnce(t)

	if engine.callCount() != 1 {
		t.Fatalf("engine ran %d times, want 1: an empty delta was reviewed", engine.callCount())
	}

	state, _ := h.st.PRState(context.Background(), "github:acme/app#7")
	if state.Cycle != 1 {
		t.Fatalf("cycle = %d, want 1 — an empty diff must not spend the budget", state.Cycle)
	}
}

func TestReReviewOfAlreadyReviewedHeadIsSkipped(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{{Summary: "p1"}}}
	h := newHarness(t, worker.Config{}, engine)

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	// Same head SHA arriving under a different delivery id (e.g. reopened).
	h.enqueue(t, "d2", "sha1", store.EventOpened)
	h.runOnce(t)

	if engine.callCount() != 1 {
		t.Fatalf("engine ran %d times for the same head", engine.callCount())
	}
}

func TestForcePushFallsBackToFullDiff(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{{Summary: "p1"}, {Summary: "p2"}}}
	h := newHarness(t, worker.Config{}, engine)

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	h.prov.compareErr = errors.New("404 no merge base")
	h.prov.pr.HeadSHA = "sha2"
	h.enqueue(t, "d2", "sha2", store.EventSynchronize)
	h.runOnce(t)

	if h.engine.request(1).Diff != "@@ full diff @@" {
		t.Fatalf("second pass diff = %q, want the full diff fallback", h.engine.request(1).Diff)
	}
}

func TestRejectedInlineCommentStillSurfacesInSummary(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{{
		Summary:  "s",
		Findings: []reviewer.Finding{finding("a.go", "nil deref", reviewer.SeverityMajor)},
	}}}

	h := newHarness(t, worker.Config{}, engine)
	h.prov.inlineErr = errors.New("422 line not part of the diff")

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	if len(h.prov.summaries) != 1 {
		t.Fatalf("summaries = %d, want 1", len(h.prov.summaries))
	}

	if !strings.Contains(h.prov.summaries[0], "No new inline comments") {
		t.Fatalf("summary should report that nothing was anchored:\n%s", h.prov.summaries[0])
	}

	state, _ := h.st.PRState(context.Background(), "github:acme/app#7")
	if state.Cycle != 1 {
		t.Fatalf("cycle = %d; a rejected anchor must not fail the whole pass", state.Cycle)
	}
}

func TestEngineFailureIsRetriedThenDeadLetteredWithANote(t *testing.T) {
	engine := &scriptedEngine{errs: []error{
		errors.New("boom"),
		errors.New("boom"),
	}}

	h := newHarness(t, worker.Config{
		MaxAttempts: 2,
		RetryDelay:  time.Millisecond,
	}, engine)

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	if engine.callCount() != 2 {
		t.Fatalf("engine calls = %d, want MaxAttempts=2", engine.callCount())
	}

	if len(h.prov.summaries) != 1 || !strings.Contains(h.prov.summaries[0], "Automated review failed") {
		t.Fatalf("summaries = %v, want a failure note", h.prov.summaries)
	}

	state, _ := h.st.PRState(context.Background(), "github:acme/app#7")
	if state.Cycle != 0 {
		t.Fatalf("cycle = %d, want 0: a failed review must not spend the budget", state.Cycle)
	}
}

func TestUnknownProviderIsDeadLetteredWithoutPanicking(t *testing.T) {
	engine := &scriptedEngine{}
	h := newHarness(t, worker.Config{MaxAttempts: 1, RetryDelay: time.Millisecond}, engine)

	ok, err := h.st.Enqueue(context.Background(), store.Job{
		DeliveryID: "d1",
		Provider:   "bitbucket",
		Repo:       "acme/app",
		PRNumber:   7,
		HeadSHA:    "sha1",
		Event:      store.EventOpened,
	})
	if err != nil || !ok {
		t.Fatalf("Enqueue = %v, %v", ok, err)
	}

	h.runOnce(t)

	if len(h.prov.summaries) != 0 {
		t.Fatalf("summaries = %v, want none for an unknown provider", h.prov.summaries)
	}
}

func TestSameCycleRetryUpdatesTheSummaryInPlace(t *testing.T) {
	// Cycle 1 succeeds but persisting fails is hard to force; instead assert
	// the update path directly: a second job at the same head after a state
	// rewrite that keeps the summary id.
	engine := &scriptedEngine{results: []reviewer.Result{{Summary: "p1"}, {Summary: "p1 again"}}}
	h := newHarness(t, worker.Config{}, engine)

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	// Simulate a crash after posting the summary but before the cycle was
	// committed: cycle 0, but the summary id and cycle marker survive.
	if err := h.st.SavePRState(context.Background(), "github:acme/app#7", store.PRState{
		SummaryCommentID: "summary-1",
		SummaryCycle:     1,
	}); err != nil {
		t.Fatalf("SavePRState: %v", err)
	}

	h.enqueue(t, "d2", "sha1-retry", store.EventOpened)
	h.runOnce(t)

	if len(h.prov.updates) != 1 {
		t.Fatalf("updates = %v, want the existing summary edited in place", h.prov.updates)
	}

	if len(h.prov.summaries) != 1 {
		t.Fatalf("summaries = %d, want no duplicate summary comment", len(h.prov.summaries))
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	engine := &scriptedEngine{}
	h := newHarness(t, worker.Config{PollInterval: time.Hour}, engine)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		h.worker.Run(ctx)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestNotifyWakesAnIdleWorker(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{{Summary: "p1"}}}
	h := newHarness(t, worker.Config{PollInterval: time.Hour}, engine)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)
		h.worker.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond) // let the loop reach its idle wait
	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.notify <- struct{}{}

	deadline := time.Now().Add(5 * time.Second)

	for engine.callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("notify did not wake the worker before the poll interval")
		}

		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-done
}

func TestATransientPostFailureLeavesTheFindingPostableNextPass(t *testing.T) {
	// Review feedback (PR #1): recording the fingerprint before the API call
	// meant a network blip silently suppressed the finding forever.
	engine := &scriptedEngine{results: []reviewer.Result{
		{Summary: "p1", Findings: []reviewer.Finding{finding("a.go", "nil deref", reviewer.SeverityMajor)}},
		{Summary: "p2", Findings: []reviewer.Finding{finding("a.go", "nil deref", reviewer.SeverityMajor)}},
	}}

	h := newHarness(t, worker.Config{}, engine)
	h.prov.inlineErr = errors.New("502 bad gateway")

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	if len(h.prov.inline) != 0 {
		t.Fatalf("inline = %+v, want none posted", h.prov.inline)
	}

	// Second pass, provider healthy again: the same finding must be posted.
	h.prov.inlineErr = nil
	h.prov.pr.HeadSHA = "sha2"
	h.enqueue(t, "d2", "sha2", store.EventSynchronize)
	h.runOnce(t)

	if len(h.prov.inline) != 1 || h.prov.inline[0].Path != "a.go" {
		t.Fatalf("inline = %+v; a failed post permanently suppressed the finding", h.prov.inline)
	}
}

func TestASuccessfulPostIsNotRepeatedNextPass(t *testing.T) {
	engine := &scriptedEngine{results: []reviewer.Result{
		{Summary: "p1", Findings: []reviewer.Finding{finding("a.go", "nil deref", reviewer.SeverityMajor)}},
		{Summary: "p2", Findings: []reviewer.Finding{finding("a.go", "nil deref", reviewer.SeverityMajor)}},
	}}

	h := newHarness(t, worker.Config{}, engine)

	h.enqueue(t, "d1", "sha1", store.EventOpened)
	h.runOnce(t)

	h.prov.pr.HeadSHA = "sha2"
	h.enqueue(t, "d2", "sha2", store.EventSynchronize)
	h.runOnce(t)

	if len(h.prov.inline) != 1 {
		t.Fatalf("inline = %d comments, want the finding posted exactly once", len(h.prov.inline))
	}
}
