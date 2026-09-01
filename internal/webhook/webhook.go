// Package webhook is the HTTP intake adapter. It verifies the delivery
// signature, translates the payload into a job, and enqueues it. Nothing else:
// ordering, dedup, and the review budget all live behind the queue.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dhiazfathra/go-pr-review-worker/internal/store"
)

// maxBodyBytes bounds a delivery. GitHub caps payloads at 25 MB; we only need
// the metadata, and an unbounded read is a free OOM on a small VM.
const maxBodyBytes = 2 << 20

// errIgnored marks a well-formed delivery that needs no review.
var errIgnored = errors.New("delivery ignored")

// Handler serves the provider webhook endpoints.
type Handler struct {
	// Store persists the job. It is the queue of record.
	Store *store.Store
	// Notify is pinged after a successful enqueue so an idle worker wakes
	// immediately instead of waiting for its poll interval. Never blocks.
	Notify chan<- struct{}
	// GitHubSecret is the HMAC secret configured on the GitHub webhook.
	GitHubSecret string
	// GitLabSecret is the token configured on the GitLab webhook.
	GitLabSecret string
	Logger       *slog.Logger
}

// Routes registers the webhook endpoints on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhook/github", h.serve(h.githubJob, h.verifyGitHub))
	mux.HandleFunc("POST /webhook/gitlab", h.serve(h.gitlabJob, h.verifyGitLab))
}

type (
	jobParser func(r *http.Request, body []byte) (store.Job, error)
	verifier  func(r *http.Request, body []byte) error
)

// serve is the shared intake path: read, verify, parse, enqueue.
func (h *Handler) serve(parse jobParser, verify verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			http.Error(w, "cannot read body", http.StatusBadRequest)

			return
		}

		if err := verify(r, body); err != nil {
			// Verification happens before anything else touches the payload.
			h.Logger.Warn("rejected webhook delivery", "path", r.URL.Path, "error", err)
			http.Error(w, "invalid signature", http.StatusUnauthorized)

			return
		}

		job, err := parse(r, body)

		switch {
		case errors.Is(err, errIgnored):
			w.WriteHeader(http.StatusNoContent)

			return
		case err != nil:
			h.Logger.Warn("unparsable webhook delivery", "path", r.URL.Path, "error", err)
			http.Error(w, "unparsable payload", http.StatusBadRequest)

			return
		}

		enqueued, err := h.Store.Enqueue(r.Context(), job)
		if err != nil {
			h.Logger.Error("enqueue failed", "delivery", job.DeliveryID, "error", err)
			http.Error(w, "enqueue failed", http.StatusInternalServerError)

			return
		}

		if !enqueued {
			// Redelivery of a job already queued or already reviewed.
			h.Logger.Info("duplicate delivery ignored", "delivery", job.DeliveryID)
			w.WriteHeader(http.StatusOK)

			return
		}

		h.Logger.Info(
			"job enqueued",
			"delivery", job.DeliveryID,
			"pr", job.PRKey(),
			"event", string(job.Event),
			"head", job.HeadSHA,
		)

		select {
		case h.Notify <- struct{}{}:
		default: // worker is busy or already has a pending wake-up
		}

		w.WriteHeader(http.StatusAccepted)
	}
}

// verifyGitHub checks the X-Hub-Signature-256 HMAC over the raw body.
func (h *Handler) verifyGitHub(r *http.Request, body []byte) error {
	if h.GitHubSecret == "" {
		return errors.New("github webhook secret not configured")
	}

	got := r.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(got, "sha256=") {
		return errors.New("missing sha256 signature header")
	}

	mac := hmac.New(sha256.New, []byte(h.GitHubSecret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return errors.New("signature mismatch")
	}

	return nil
}

// verifyGitLab compares the shared token in constant time. GitLab has no HMAC
// mode, so the token is the whole authentication story — it must be long.
func (h *Handler) verifyGitLab(r *http.Request, _ []byte) error {
	if h.GitLabSecret == "" {
		return errors.New("gitlab webhook secret not configured")
	}

	got := r.Header.Get("X-Gitlab-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.GitLabSecret)) != 1 {
		return errors.New("token mismatch")
	}

	return nil
}

// githubJob maps a pull_request delivery to a job.
func (h *Handler) githubJob(r *http.Request, body []byte) (store.Job, error) {
	if event := r.Header.Get("X-GitHub-Event"); event != "pull_request" {
		return store.Job{}, fmt.Errorf("%w: event %q", errIgnored, event)
	}

	var payload struct {
		Action      string `json:"action"`
		Number      int    `json:"number"`
		PullRequest struct {
			Number int `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				SHA string `json:"sha"`
			} `json:"base"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		return store.Job{}, fmt.Errorf("decoding github payload: %w", err)
	}

	var event store.Event

	switch payload.Action {
	case "opened", "reopened", "ready_for_review":
		event = store.EventOpened
	case "synchronize":
		event = store.EventSynchronize
	default:
		return store.Job{}, fmt.Errorf("%w: action %q", errIgnored, payload.Action)
	}

	number := payload.PullRequest.Number
	if number == 0 {
		number = payload.Number
	}

	if payload.PullRequest.Head.SHA == "" {
		return store.Job{}, errors.New("payload has no head sha")
	}

	return store.Job{
		// Keying on head SHA rather than the delivery UUID makes a GitHub
		// redelivery (which reuses the UUID) and a duplicate push event
		// collapse to the same job.
		DeliveryID: fmt.Sprintf("github:%s#%d:%s", payload.Repository.FullName, number, payload.PullRequest.Head.SHA),
		Provider:   "github",
		Repo:       payload.Repository.FullName,
		PRNumber:   number,
		HeadSHA:    payload.PullRequest.Head.SHA,
		BaseSHA:    payload.PullRequest.Base.SHA,
		Event:      event,
	}, nil
}

// gitlabJob maps a Merge Request Hook delivery to a job.
func (h *Handler) gitlabJob(r *http.Request, body []byte) (store.Job, error) {
	if event := r.Header.Get("X-Gitlab-Event"); event != "Merge Request Hook" {
		return store.Job{}, fmt.Errorf("%w: event %q", errIgnored, event)
	}

	var payload struct {
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		ObjectAttributes struct {
			IID        int    `json:"iid"`
			Action     string `json:"action"`
			OldRev     string `json:"oldrev"`
			LastCommit struct {
				ID string `json:"id"`
			} `json:"last_commit"`
			DiffRefs struct {
				BaseSHA string `json:"base_sha"`
				HeadSHA string `json:"head_sha"`
			} `json:"diff_refs"`
		} `json:"object_attributes"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		return store.Job{}, fmt.Errorf("decoding gitlab payload: %w", err)
	}

	attrs := payload.ObjectAttributes

	var event store.Event

	switch {
	case attrs.Action == "open" || attrs.Action == "reopen":
		event = store.EventOpened
	case attrs.Action == "update" && attrs.OldRev != "":
		// oldrev is present only when the update pushed new commits; a label
		// or title edit must not spend a review cycle.
		event = store.EventSynchronize
	default:
		return store.Job{}, fmt.Errorf("%w: action %q", errIgnored, attrs.Action)
	}

	head := attrs.DiffRefs.HeadSHA
	if head == "" {
		head = attrs.LastCommit.ID
	}

	return store.Job{
		DeliveryID: fmt.Sprintf("gitlab:%s#%d:%s", payload.Project.PathWithNamespace, attrs.IID, head),
		Provider:   "gitlab",
		Repo:       payload.Project.PathWithNamespace,
		PRNumber:   attrs.IID,
		HeadSHA:    head,
		BaseSHA:    attrs.DiffRefs.BaseSHA,
		Event:      event,
	}, nil
}
