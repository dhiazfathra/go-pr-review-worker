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

const marker = "<!-- pr-review-worker -->"

// threadProvider is a fakeProvider that can also read, answer, resolve and
// approve — the ThreadReviewer capability the follow-up pass needs.
type threadProvider struct {
	fakeProvider

	tmu     sync.Mutex
	threads []provider.ReviewThread

	resolved   []string
	replies    []string
	approvals  []string
	resolveErr error
	approveErr error
}

func (p *threadProvider) ReviewThreads(context.Context, string, int) ([]provider.ReviewThread, error) {
	p.tmu.Lock()
	defer p.tmu.Unlock()

	return p.threads, nil
}

func (p *threadProvider) ReplyToThread(_ context.Context, _ string, _ int, _, body string) error {
	p.tmu.Lock()
	defer p.tmu.Unlock()

	p.replies = append(p.replies, body)

	return nil
}

func (p *threadProvider) ResolveThread(_ context.Context, threadID string) error {
	p.tmu.Lock()
	defer p.tmu.Unlock()

	if p.resolveErr != nil {
		return p.resolveErr
	}

	p.resolved = append(p.resolved, threadID)

	return nil
}

func (p *threadProvider) Approve(_ context.Context, _ string, _ int, body string) error {
	p.tmu.Lock()
	defer p.tmu.Unlock()

	if p.approveErr != nil {
		return p.approveErr
	}

	p.approvals = append(p.approvals, body)

	return nil
}

func (p *threadProvider) snapshot() (resolved, replies, approvals []string) {
	p.tmu.Lock()
	defer p.tmu.Unlock()

	return append([]string(nil), p.resolved...),
		append([]string(nil), p.replies...),
		append([]string(nil), p.approvals...)
}

// verifyingEngine is a scriptedEngine that also answers Verify.
type verifyingEngine struct {
	scriptedEngine

	vmu       sync.Mutex
	verdicts  []reviewer.ThreadVerdict
	verifyErr error
	asked     []reviewer.VerifyRequest
}

func (e *verifyingEngine) Verify(_ context.Context, req reviewer.VerifyRequest) (reviewer.VerifyResult, error) {
	e.vmu.Lock()
	defer e.vmu.Unlock()

	e.asked = append(e.asked, req)

	if e.verifyErr != nil {
		return reviewer.VerifyResult{}, e.verifyErr
	}

	return reviewer.VerifyResult{Verdicts: e.verdicts, Engine: "scripted"}, nil
}

func (e *verifyingEngine) askedWith() []reviewer.VerifyRequest {
	e.vmu.Lock()
	defer e.vmu.Unlock()

	return append([]reviewer.VerifyRequest(nil), e.asked...)
}

func workerThread(id, path string, resolved bool, replies ...string) provider.ReviewThread {
	t := provider.ReviewThread{
		ID:   id,
		Path: path,
		Line: 30,
		// The forge reports the first comment as written by the worker's own
		// account; without that the thread is not the worker's to resolve.
		StartedByWorker: true,
		Resolved:        resolved,
		Comments: []provider.ThreadComment{
			{ID: "c-" + id, Author: "pr-review-worker", Body: marker + "\n\n**🟠 major** — original finding"},
		},
	}

	for i, r := range replies {
		t.Comments = append(t.Comments, provider.ThreadComment{
			ID:     "r-" + id,
			Author: "author",
			Body:   r,
		})
		_ = i
	}

	return t
}

// verifyFixture wires a worker whose provider and engine both support the
// follow-up pass, with pass 1 already recorded at sha1.
func verifyFixture(t *testing.T, cfg worker.Config, engine *verifyingEngine, prov *threadProvider) (*store.Store, *worker.Worker) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	if err := st.SavePRState(context.Background(), "github:o/r#7", store.PRState{
		Cycle:           1,
		LastReviewedSHA: "sha1",
	}); err != nil {
		t.Fatalf("SavePRState: %v", err)
	}

	w := worker.New(
		st,
		engine,
		map[string]provider.Provider{"github": prov},
		make(chan struct{}, 1),
		cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	return st, w
}

// runJob enqueues one synchronize job at sha2 and drains it.
func runJob(t *testing.T, st *store.Store, w *worker.Worker) {
	t.Helper()

	if _, err := st.Enqueue(context.Background(), store.Job{
		DeliveryID: "github:o/r#7:sha2",
		Provider:   "github",
		Repo:       "o/r",
		PRNumber:   7,
		HeadSHA:    "sha2",
		Event:      store.EventSynchronize,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	deadline := time.Now().Add(10 * time.Second)

	for {
		n, err := st.PendingCount(context.Background())
		if err != nil {
			cancel()
			<-done
			t.Fatalf("PendingCount: %v", err)
		}

		if n == 0 {
			break
		}

		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("queue did not drain")
		}

		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-done
}

func newThreadProvider(threads ...provider.ReviewThread) *threadProvider {
	p := &threadProvider{threads: threads}
	p.pr = provider.PullRequest{Title: "Add trace id", Body: "desc", HeadSHA: "sha2"}
	p.fullDiff = "@@ full @@"

	// The delta diff has to name every thread's file: resolving a thread
	// requires the diff to have touched the file the finding sits on, so a
	// fixture with a placeholder diff would make every verdict unresolvable.
	var b strings.Builder

	b.WriteString("@@ delta @@\n")

	for _, t := range threads {
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", t.Path, t.Path)
	}

	p.compareDiff = b.String()

	return p
}

func TestVerifyResolvesAFixedThreadAndLeavesAnUnfixedOneOpen(t *testing.T) {
	prov := newThreadProvider(
		workerThread("T1", "a.ts", false, "Fixed in 783950b"),
		workerThread("T2", "b.ts", false, "Fixed in 783950b"),
	)

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
		verdicts: []reviewer.ThreadVerdict{
			{ID: "T1", Verdict: reviewer.VerdictFixed, Note: "done"},
			{ID: "T2", Verdict: reviewer.VerdictUnfixed, Note: "the array branch still returns early"},
		},
	}

	st, w := verifyFixture(t, worker.Config{VerifyReplies: true}, engine, prov)
	runJob(t, st, w)

	resolved, replies, _ := prov.snapshot()

	if len(resolved) != 1 || resolved[0] != "T1" {
		t.Fatalf("resolved = %v, want only T1", resolved)
	}

	var followUp string

	for _, r := range replies {
		if strings.Contains(r, "Still open") {
			followUp = r
		}
	}

	if followUp == "" {
		t.Fatal("no follow-up reply posted for the thread that is still broken")
	}

	if !strings.Contains(followUp, "the array branch still returns early") {
		t.Errorf("follow-up reply does not carry the engine's reason: %q", followUp)
	}
}

// A reply claiming "fixed" must not be enough on its own — the engine judges
// the diff, and an unfixed verdict has to keep the thread open.
func TestVerifyDoesNotResolveOnTheAuthorsWordAlone(t *testing.T) {
	prov := newThreadProvider(workerThread("T1", "a.ts", false, "Fixed, promise!"))

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
		verdicts: []reviewer.ThreadVerdict{
			{ID: "T1", Verdict: reviewer.VerdictUnfixed, Note: "nothing changed here"},
		},
	}

	st, w := verifyFixture(t, worker.Config{VerifyReplies: true}, engine, prov)
	runJob(t, st, w)

	resolved, _, _ := prov.snapshot()
	if len(resolved) != 0 {
		t.Fatalf("resolved = %v, want none: a claim of 'fixed' is not evidence", resolved)
	}
}

func TestVerifySkipsThreadsTheWorkerDidNotOpen(t *testing.T) {
	human := provider.ReviewThread{
		ID:   "H1",
		Path: "a.ts",
		Line: 10,
		Comments: []provider.ThreadComment{
			{ID: "c1", Author: "someone", Body: "please rename this"},
		},
	}

	prov := newThreadProvider(human, workerThread("T1", "a.ts", true))

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
	}

	st, w := verifyFixture(t, worker.Config{VerifyReplies: true}, engine, prov)
	runJob(t, st, w)

	if got := engine.askedWith(); len(got) != 0 {
		t.Fatalf("engine was asked to verify %d time(s); a human thread and a resolved one are not its business", len(got))
	}
}

func TestVerifyPassesRepliesAndTheDeltaDiffToTheEngine(t *testing.T) {
	prov := newThreadProvider(workerThread("T1", "a.ts", false, "Fixed in 783950b — added a guard"))

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
		verdicts: []reviewer.ThreadVerdict{
			{ID: "T1", Verdict: reviewer.VerdictFixed},
		},
	}

	st, w := verifyFixture(t, worker.Config{VerifyReplies: true}, engine, prov)
	runJob(t, st, w)

	asked := engine.askedWith()
	if len(asked) != 1 {
		t.Fatalf("Verify called %d times, want 1", len(asked))
	}

	req := asked[0]
	if req.Diff != prov.compareDiff {
		t.Errorf("Diff = %q, want the diff since the reviewed commit", req.Diff)
	}

	if !strings.HasPrefix(req.Diff, "@@ delta @@") {
		t.Errorf("Diff = %q, want the compare diff rather than the full one", req.Diff)
	}

	if len(req.Threads) != 1 || len(req.Threads[0].Replies) != 1 {
		t.Fatalf("threads = %+v, want one thread carrying the author's reply", req.Threads)
	}

	if !strings.Contains(req.Threads[0].Replies[0], "added a guard") {
		t.Errorf("reply not forwarded to the engine: %q", req.Threads[0].Replies[0])
	}
}

func TestApproveOnlyWhenEverythingIsResolvedAndEnabled(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		verdict     reviewer.Verdict
		wantApprove bool
	}{
		{"all fixed and enabled", true, reviewer.VerdictFixed, true},
		{"all fixed but disabled", false, reviewer.VerdictFixed, false},
		{"enabled but a thread is still open", true, reviewer.VerdictUnfixed, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov := newThreadProvider(workerThread("T1", "a.ts", false, "Fixed"))

			engine := &verifyingEngine{
				// An empty second pass: nothing new found, so approving is
				// only about whether the old threads are closed.
				scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
				verdicts:       []reviewer.ThreadVerdict{{ID: "T1", Verdict: tc.verdict, Note: "n"}},
			}

			st, w := verifyFixture(t, worker.Config{
				VerifyReplies:       true,
				ApproveWhenResolved: tc.enabled,
			}, engine, prov)

			runJob(t, st, w)

			_, _, approvals := prov.snapshot()
			if got := len(approvals) > 0; got != tc.wantApprove {
				t.Fatalf("approved = %v, want %v (approvals: %v)", got, tc.wantApprove, approvals)
			}
		})
	}
}

// A new finding in the same pass contradicts an approval, so the worker must
// not do both at once.
func TestApprovalIsWithheldWhenTheSamePassFindsSomethingNew(t *testing.T) {
	prov := newThreadProvider(workerThread("T1", "a.ts", false, "Fixed"))

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{{
			Summary:  "pass 2",
			Findings: []reviewer.Finding{finding("c.ts", "new problem", reviewer.SeverityMajor)},
		}}},
		verdicts: []reviewer.ThreadVerdict{{ID: "T1", Verdict: reviewer.VerdictFixed}},
	}

	st, w := verifyFixture(t, worker.Config{
		VerifyReplies:       true,
		ApproveWhenResolved: true,
	}, engine, prov)

	runJob(t, st, w)

	_, _, approvals := prov.snapshot()
	if len(approvals) != 0 {
		t.Fatalf("approved while reporting a new finding: %v", approvals)
	}
}

func TestVerifySkippedWhenDisabled(t *testing.T) {
	prov := newThreadProvider(workerThread("T1", "a.ts", false, "Fixed"))

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
		verdicts:       []reviewer.ThreadVerdict{{ID: "T1", Verdict: reviewer.VerdictFixed}},
	}

	st, w := verifyFixture(t, worker.Config{VerifyReplies: false}, engine, prov)
	runJob(t, st, w)

	if got := engine.askedWith(); len(got) != 0 {
		t.Fatalf("Verify ran %d time(s) with PRW_VERIFY_REPLIES off", len(got))
	}
}

// A thread that cannot be resolved must stay open rather than be counted as
// fixed, or the next pass would never retry it and the PR could be approved
// with a live finding on it.
func TestUnresolvableThreadIsNotCountedAsFixed(t *testing.T) {
	prov := newThreadProvider(workerThread("T1", "a.ts", false, "Fixed"))
	prov.resolveErr = errors.New("thread locked")

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
		verdicts:       []reviewer.ThreadVerdict{{ID: "T1", Verdict: reviewer.VerdictFixed}},
	}

	st, w := verifyFixture(t, worker.Config{
		VerifyReplies:       true,
		ApproveWhenResolved: true,
	}, engine, prov)

	runJob(t, st, w)

	_, _, approvals := prov.snapshot()
	if len(approvals) != 0 {
		t.Fatalf("approved despite a thread that could not be resolved: %v", approvals)
	}
}

// The marker is text in a comment body, so anyone can paste one. Ownership has
// to come from the forge's own view of who wrote the comment, or a human could
// open a thread that the worker would then adopt and resolve.
func TestAThreadCarryingTheMarkerButWrittenByAHumanIsIgnored(t *testing.T) {
	thread := workerThread("T1", "a.ts", false, "Fixed")
	thread.StartedByWorker = false
	thread.Comments[0].Author = "impostor"

	prov := newThreadProvider(thread)

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
		verdicts:       []reviewer.ThreadVerdict{{ID: "T1", Verdict: reviewer.VerdictFixed}},
	}

	st, w := verifyFixture(t, worker.Config{
		VerifyReplies:       true,
		ApproveWhenResolved: true,
	}, engine, prov)

	runJob(t, st, w)

	if got := engine.askedWith(); len(got) != 0 {
		t.Fatalf("Verify ran on a thread the worker did not write: %+v", got)
	}

	resolved, _, approvals := prov.snapshot()
	if len(resolved) != 0 {
		t.Errorf("resolved %v, want nothing: the thread is not the worker's", resolved)
	}

	if len(approvals) != 0 {
		t.Errorf("approved over an open thread it does not own: %v", approvals)
	}
}

// The engine reads the diff and the replies, both of which the PR author
// controls, so a `fixed` verdict is not authority on its own. If the commits
// since the last review never touched the file, nothing on it can be fixed.
func TestAFixedVerdictForAFileTheDiffDoesNotTouchIsIgnored(t *testing.T) {
	prov := newThreadProvider(workerThread("T1", "untouched.ts", false, "Fixed, honest"))

	// Overwrite the fixture's diff so it names a different file than the
	// thread's: this is the shape of an injected verdict.
	prov.compareDiff = "@@ delta @@\n--- a/other.ts\n+++ b/other.ts\n"

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
		verdicts:       []reviewer.ThreadVerdict{{ID: "T1", Verdict: reviewer.VerdictFixed}},
	}

	st, w := verifyFixture(t, worker.Config{
		VerifyReplies:       true,
		ApproveWhenResolved: true,
	}, engine, prov)

	runJob(t, st, w)

	resolved, _, approvals := prov.snapshot()
	if len(resolved) != 0 {
		t.Errorf("resolved %v on a file the diff never touched", resolved)
	}

	if len(approvals) != 0 {
		t.Errorf("approved with the thread still open: %v", approvals)
	}
}

// A forge that refuses the approval must not fail the job. The refusal is
// permanent — the token's account opened the PR, or it cannot review the
// repository — so retrying it would eventually dead-letter a PR whose findings
// were posted and whose threads were resolved, and tell the author the review
// failed when it did not.
func TestARefusedApprovalLeavesTheJobSuccessful(t *testing.T) {
	prov := newThreadProvider(workerThread("T1", "a.ts", false, "Fixed"))
	prov.approveErr = &provider.StatusError{
		Method: "POST",
		Path:   "/repos/o/r/pulls/7/reviews",
		Code:   422,
		Body:   `{"errors":["Review Can not approve your own pull request"]}`,
	}

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
		verdicts:       []reviewer.ThreadVerdict{{ID: "T1", Verdict: reviewer.VerdictFixed}},
	}

	st, w := verifyFixture(t, worker.Config{
		VerifyReplies:       true,
		ApproveWhenResolved: true,
	}, engine, prov)

	// runJob drains the queue or fails the test, so reaching the assertions at
	// all is the proof that the refusal did not put the job into retry.
	runJob(t, st, w)

	resolved, _, approvals := prov.snapshot()
	if len(resolved) != 1 {
		t.Errorf("resolved %v, want the thread resolved despite the refused approval", resolved)
	}

	if len(approvals) != 0 {
		t.Errorf("recorded an approval the forge refused: %v", approvals)
	}

	state, err := st.PRState(context.Background(), "github:o/r#7")
	if err != nil {
		t.Fatalf("PRState: %v", err)
	}

	if state.Approved {
		t.Error("PR marked approved after the forge refused the approval")
	}
}

// A transient approval failure is a different thing from a refusal: nothing
// about a 503 says the approval is disallowed, so it must be treated as a job
// failure and retried rather than recorded as "permanently unapproved".
func TestATransientApprovalFailureFailsTheJob(t *testing.T) {
	prov := newThreadProvider(workerThread("T1", "a.ts", false, "Fixed"))
	prov.approveErr = &provider.StatusError{
		Method: "POST",
		Path:   "/repos/o/r/pulls/7/reviews",
		Code:   503,
		Body:   "upstream unavailable",
	}

	engine := &verifyingEngine{
		scriptedEngine: scriptedEngine{results: []reviewer.Result{
			{Summary: "pass 2"},
			{Summary: "pass 2 retry"},
		}},
		verdicts: []reviewer.ThreadVerdict{{ID: "T1", Verdict: reviewer.VerdictFixed}},
	}

	// MaxAttempts 1 makes the first failure terminal, so the queue still
	// drains and the outcome is observable as a dead job rather than a
	// permanently spinning retry.
	st, w := verifyFixture(t, worker.Config{
		VerifyReplies:       true,
		ApproveWhenResolved: true,
		MaxAttempts:         1,
		RetryDelay:          time.Millisecond,
	}, engine, prov)

	runJob(t, st, w)

	if _, _, approvals := prov.snapshot(); len(approvals) != 0 {
		t.Errorf("recorded an approval that never succeeded: %v", approvals)
	}

	// A dead-lettered job is the proof the error propagated. A permanent
	// refusal takes the other branch and leaves the job successful
	// (TestARefusedApprovalLeavesTheJobSuccessful).
	notices := prov.summaryBodies()

	var failed bool

	for _, n := range notices {
		if strings.Contains(n, "Automated review failed") {
			failed = true
		}
	}

	if !failed {
		t.Errorf("job completed successfully; a 503 approval must fail it. summaries: %v", notices)
	}
}

// A finding on a file the follow-up commits deleted or renamed is fixed by
// definition, but the thread still carries the old path. Reading only the
// post-image side of the diff made both look untouched, so the correct verdict
// was discarded and the thread stayed open forever.
func TestAFixedVerdictIsAcceptedWhenTheFileWasDeletedOrRenamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		diff string
	}{
		{"deleted", "@@ delta @@\n--- a/gone.ts\n+++ /dev/null\n"},
		{"renamed", "@@ delta @@\n--- a/gone.ts\n+++ b/renamed.ts\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := newThreadProvider(workerThread("T1", "gone.ts", false, "Deleted it"))
			prov.compareDiff = tc.diff

			engine := &verifyingEngine{
				scriptedEngine: scriptedEngine{results: []reviewer.Result{{Summary: "pass 2"}}},
				verdicts:       []reviewer.ThreadVerdict{{ID: "T1", Verdict: reviewer.VerdictFixed}},
			}

			st, w := verifyFixture(t, worker.Config{VerifyReplies: true}, engine, prov)
			runJob(t, st, w)

			resolved, _, _ := prov.snapshot()
			if len(resolved) != 1 || resolved[0] != "T1" {
				t.Fatalf("resolved = %v, want T1: the diff does touch its path", resolved)
			}
		})
	}
}

// The follow-up is what the worker still owes a PR whose review budget is
// spent; refusing to run it there would strand every thread on a busy PR.
func TestVerifyStillRunsAfterTheReviewBudgetIsSpent(t *testing.T) {
	prov := newThreadProvider(workerThread("T1", "a.ts", false, "Fixed"))

	engine := &verifyingEngine{
		verdicts: []reviewer.ThreadVerdict{{ID: "T1", Verdict: reviewer.VerdictFixed}},
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "spent.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	// Both passes already used.
	if err := st.SavePRState(context.Background(), "github:o/r#7", store.PRState{
		Cycle:           2,
		LastReviewedSHA: "sha1",
	}); err != nil {
		t.Fatalf("SavePRState: %v", err)
	}

	w := worker.New(
		st,
		engine,
		map[string]provider.Provider{"github": prov},
		make(chan struct{}, 1),
		worker.Config{MaxCycles: 2, VerifyReplies: true, AnnounceBudgetExhausted: false},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	runJob(t, st, w)

	resolved, _, _ := prov.snapshot()
	if len(resolved) != 1 {
		t.Fatalf("resolved = %v, want T1 resolved even with the budget spent", resolved)
	}

	if engine.callCount() != 0 {
		t.Errorf("Review ran %d time(s) with the budget spent; only the follow-up should have", engine.callCount())
	}
}
