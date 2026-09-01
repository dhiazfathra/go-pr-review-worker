package webhook_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhiazfathra/go-pr-review-worker/internal/store"
	"github.com/dhiazfathra/go-pr-review-worker/internal/webhook"
)

const (
	githubSecret = "gh-secret"
	gitlabSecret = "gl-secret-token"
)

func harness(t *testing.T) (*http.ServeMux, *store.Store, chan struct{}) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "wh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	notify := make(chan struct{}, 1)
	h := &webhook.Handler{
		Store:        st,
		Notify:       notify,
		GitHubSecret: githubSecret,
		GitLabSecret: gitlabSecret,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	h.Routes(mux)

	return mux, st, notify
}

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(githubSecret))
	mac.Write([]byte(body))

	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func githubPayload(action, sha string) string {
	return `{"action":"` + action + `","pull_request":{"number":7,` +
		`"head":{"sha":"` + sha + `"},"base":{"sha":"base1"}},` +
		`"repository":{"full_name":"acme/app"}}`
}

func postGitHub(t *testing.T, mux *http.ServeMux, event, body, signature string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "delivery-1")
	req.Header.Set("X-Hub-Signature-256", signature)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	return rec
}

func TestGitHubDeliveryEnqueuesAndNotifies(t *testing.T) {
	mux, st, notify := harness(t)
	body := githubPayload("opened", "sha1")

	rec := postGitHub(t, mux, "pull_request", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body)
	}

	select {
	case <-notify:
	default:
		t.Fatal("worker was not notified")
	}

	job, err := st.ClaimNext(t.Context())
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if job.Repo != "acme/app" || job.PRNumber != 7 || job.HeadSHA != "sha1" {
		t.Fatalf("job = %+v", job)
	}

	if job.Event != store.EventOpened {
		t.Fatalf("event = %q, want opened", job.Event)
	}
}

func TestGitHubBadSignatureIsRejectedBeforeEnqueue(t *testing.T) {
	mux, st, _ := harness(t)
	body := githubPayload("opened", "sha1")

	rec := postGitHub(t, mux, "pull_request", body, sign("other body"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if n, _ := st.PendingCount(t.Context()); n != 0 {
		t.Fatalf("pending = %d, want 0: an unverified payload reached the queue", n)
	}
}

func TestGitHubMissingSignatureHeaderIsRejected(t *testing.T) {
	mux, _, _ := harness(t)
	body := githubPayload("opened", "sha1")

	if rec := postGitHub(t, mux, "pull_request", body, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGitHubRedeliveryOfSameHeadIsDeduped(t *testing.T) {
	mux, st, _ := harness(t)
	body := githubPayload("opened", "sha1")

	if rec := postGitHub(t, mux, "pull_request", body, sign(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("first status = %d", rec.Code)
	}

	if rec := postGitHub(t, mux, "pull_request", body, sign(body)); rec.Code != http.StatusOK {
		t.Fatalf("redelivery status = %d, want 200", rec.Code)
	}

	if n, _ := st.PendingCount(t.Context()); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}
}

func TestGitHubIgnoredActionsAndEvents(t *testing.T) {
	mux, st, _ := harness(t)

	cases := []struct{ event, body string }{
		{"issues", githubPayload("opened", "sha1")},
		{"pull_request", githubPayload("labeled", "sha1")},
		{"pull_request", githubPayload("closed", "sha1")},
	}

	for _, c := range cases {
		rec := postGitHub(t, mux, c.event, c.body, sign(c.body))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("event %q: status = %d, want 204", c.event, rec.Code)
		}
	}

	if n, _ := st.PendingCount(t.Context()); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
}

func TestGitHubSynchronizeMapsToSecondCycleEvent(t *testing.T) {
	mux, st, _ := harness(t)
	body := githubPayload("synchronize", "sha2")

	if rec := postGitHub(t, mux, "pull_request", body, sign(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	job, err := st.ClaimNext(t.Context())
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if job.Event != store.EventSynchronize {
		t.Fatalf("event = %q, want synchronize", job.Event)
	}
}

func TestGitHubMalformedPayloadAndMissingSHA(t *testing.T) {
	mux, _, _ := harness(t)

	broken := "{not json"
	if rec := postGitHub(t, mux, "pull_request", broken, sign(broken)); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400", rec.Code)
	}

	noSHA := `{"action":"opened","pull_request":{"number":7},"repository":{"full_name":"acme/app"}}`
	if rec := postGitHub(t, mux, "pull_request", noSHA, sign(noSHA)); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing sha status = %d, want 400", rec.Code)
	}
}

func TestGitHubNumberFallsBackToTopLevelField(t *testing.T) {
	mux, st, _ := harness(t)
	body := `{"action":"opened","number":11,"pull_request":{"head":{"sha":"s"},` +
		`"base":{"sha":"b"}},"repository":{"full_name":"acme/app"}}`

	if rec := postGitHub(t, mux, "pull_request", body, sign(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	job, err := st.ClaimNext(t.Context())
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if job.PRNumber != 11 {
		t.Fatalf("PRNumber = %d, want 11", job.PRNumber)
	}
}

func postGitLab(t *testing.T, mux *http.ServeMux, event, token, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", strings.NewReader(body))
	req.Header.Set("X-Gitlab-Event", event)
	req.Header.Set("X-Gitlab-Token", token)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	return rec
}

func gitlabPayload(action, oldrev string) string {
	return `{"project":{"path_with_namespace":"grp/proj"},"object_attributes":{"iid":4,` +
		`"action":"` + action + `","oldrev":"` + oldrev + `",` +
		`"last_commit":{"id":"fallback-sha"},` +
		`"diff_refs":{"base_sha":"b1","head_sha":"h1"}}}`
}

func TestGitLabOpenEnqueues(t *testing.T) {
	mux, st, _ := harness(t)

	rec := postGitLab(t, mux, "Merge Request Hook", gitlabSecret, gitlabPayload("open", ""))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body)
	}

	job, err := st.ClaimNext(t.Context())
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if job.Provider != "gitlab" || job.Repo != "grp/proj" || job.PRNumber != 4 || job.HeadSHA != "h1" {
		t.Fatalf("job = %+v", job)
	}
}

func TestGitLabUpdateWithoutOldRevIsIgnored(t *testing.T) {
	mux, st, _ := harness(t)

	// A label or title edit arrives as "update" with no oldrev; spending a
	// review cycle on it would waste the budget.
	rec := postGitLab(t, mux, "Merge Request Hook", gitlabSecret, gitlabPayload("update", ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	if n, _ := st.PendingCount(t.Context()); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
}

func TestGitLabUpdateWithOldRevIsSynchronize(t *testing.T) {
	mux, st, _ := harness(t)

	rec := postGitLab(t, mux, "Merge Request Hook", gitlabSecret, gitlabPayload("update", "old1"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	job, err := st.ClaimNext(t.Context())
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if job.Event != store.EventSynchronize {
		t.Fatalf("event = %q", job.Event)
	}
}

func TestGitLabHeadFallsBackToLastCommit(t *testing.T) {
	mux, st, _ := harness(t)
	body := `{"project":{"path_with_namespace":"grp/proj"},"object_attributes":{"iid":4,` +
		`"action":"open","last_commit":{"id":"fallback-sha"}}}`

	if rec := postGitLab(t, mux, "Merge Request Hook", gitlabSecret, body); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	job, err := st.ClaimNext(t.Context())
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}

	if job.HeadSHA != "fallback-sha" {
		t.Fatalf("HeadSHA = %q", job.HeadSHA)
	}
}

func TestGitLabWrongTokenAndWrongEvent(t *testing.T) {
	mux, _, _ := harness(t)

	if rec := postGitLab(t, mux, "Merge Request Hook", "nope", gitlabPayload("open", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want 401", rec.Code)
	}

	if rec := postGitLab(t, mux, "Push Hook", gitlabSecret, gitlabPayload("open", "")); rec.Code != http.StatusNoContent {
		t.Fatalf("wrong event status = %d, want 204", rec.Code)
	}

	if rec := postGitLab(t, mux, "Merge Request Hook", gitlabSecret, "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400", rec.Code)
	}
}

func TestUnconfiguredSecretsRejectEverything(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "wh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	h := &webhook.Handler{
		Store:  st,
		Notify: make(chan struct{}, 1),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	h.Routes(mux)

	body := githubPayload("opened", "s")
	if rec := postGitHub(t, mux, "pull_request", body, sign(body)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("github status = %d, want 401", rec.Code)
	}

	if rec := postGitLab(t, mux, "Merge Request Hook", "", gitlabPayload("open", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("gitlab status = %d, want 401", rec.Code)
	}
}

func TestEnqueueFailureReturns500(t *testing.T) {
	mux, st, _ := harness(t)

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body := githubPayload("opened", "sha1")
	if rec := postGitHub(t, mux, "pull_request", body, sign(body)); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestFullNotifyChannelDoesNotBlock(t *testing.T) {
	mux, _, notify := harness(t)
	notify <- struct{}{} // buffer already holds a pending wake-up

	body := githubPayload("opened", "sha1")
	if rec := postGitHub(t, mux, "pull_request", body, sign(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestOversizedBodyIsRejectedAsTooLargeNotAsABadSignature(t *testing.T) {
	// Review feedback (PR #1): truncating before HMAC made a size problem look
	// like a secret mismatch, sending operators after the wrong cause.
	mux, st, _ := harness(t)

	body := `{"action":"opened","pull_request":{"number":7,"head":{"sha":"s"},` +
		`"base":{"sha":"b"}},"repository":{"full_name":"acme/app"},"pad":"` +
		strings.Repeat("x", 3<<20) + `"}`

	rec := postGitHub(t, mux, "pull_request", body, sign(body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}

	if n, _ := st.PendingCount(t.Context()); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
}
