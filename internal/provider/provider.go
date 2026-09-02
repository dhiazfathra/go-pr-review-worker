// Package provider wraps the forge REST APIs the worker talks to. Only the
// handful of calls a review needs are implemented, so there is no SDK
// dependency and the binary stays small.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PullRequest is the subset of PR metadata a review needs.
type PullRequest struct {
	Title   string
	Body    string
	HeadSHA string
	BaseSHA string
}

// InlineComment is one review comment anchored to a line of the new file.
type InlineComment struct {
	Path string
	Line int
	Body string
}

// OpenPullRequest is the minimum the watcher needs to decide whether a pull
// request has moved on since its last review.
type OpenPullRequest struct {
	Number  int
	HeadSHA string
	BaseSHA string
}

// ThreadComment is one comment in a review thread, in posting order.
type ThreadComment struct {
	// ID is the provider id of the comment, used to reply into the thread.
	ID string
	// Author is the login that wrote it, so the worker can tell its own
	// findings apart from the author's replies.
	Author string
	Body   string
}

// ReviewThread is one inline conversation on a pull request.
type ReviewThread struct {
	// ID is the provider handle used to resolve the thread. On GitHub this is
	// a GraphQL node id, which is why resolving is not a REST call.
	ID       string
	Path     string
	Line     int
	Resolved bool
	Comments []ThreadComment
	// StartedByWorker reports that the thread's first comment was written by
	// the account the worker is authenticated as. A marker in the body cannot
	// establish that — anyone can paste one — so ownership is decided by the
	// forge's own view of who wrote the comment.
	StartedByWorker bool
}

// StatusError is a non-2xx response. It carries the status code so a caller
// can tell a permanent refusal from a transient one: retrying a `422` is
// pointless, retrying a `502` is the right thing to do.
type StatusError struct {
	Method string
	Path   string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: unexpected status %d: %s", e.Method, e.Path, e.Code, e.Body)
}

// Permanent reports whether retrying the same request could ever succeed.
// Everything the forge answers in the 4xx range is a statement about the
// request itself — bad credentials, missing scope, a rule that forbids the
// action — except `408` and `429`, which are about timing. Anything else
// (5xx, transport errors) is treated as transient.
func (e *StatusError) Permanent() bool {
	switch e.Code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	default:
		return e.Code >= 400 && e.Code < 500
	}
}

// ErrTooManyResults is returned by a list call whose paging cap was reached
// while the forge still had more to give. Returning the truncated list as if
// it were complete would make the watcher silently ignore every pull request
// past the cap.
var ErrTooManyResults = errors.New("provider: more results than the page limit allows")

// Provider is a forge the worker can read diffs from and post reviews to.
type Provider interface {
	Name() string
	// PullRequest fetches PR metadata.
	PullRequest(ctx context.Context, repo string, number int) (PullRequest, error)
	// ListOpenPullRequests returns every open pull request, for the watcher to
	// compare against what has already been reviewed.
	ListOpenPullRequests(ctx context.Context, repo string) ([]OpenPullRequest, error)
	// Diff returns the unified diff of the whole PR.
	Diff(ctx context.Context, repo string, number int) (string, error)
	// CompareDiff returns the unified diff between two commits, used to scope
	// the second review pass to what changed since the first.
	CompareDiff(ctx context.Context, repo, from, to string) (string, error)
	// PostInline posts one inline comment on the PR at headSHA.
	PostInline(ctx context.Context, repo string, number int, headSHA string, c InlineComment) error
	// PostSummary posts the summary comment and returns its provider id.
	PostSummary(ctx context.Context, repo string, number int, body string) (string, error)
	// UpdateSummary edits a previously posted summary comment in place.
	UpdateSummary(ctx context.Context, repo, commentID, body string) error
}

// ThreadReviewer is the optional capability the follow-up pass needs: reading
// the worker's own threads, answering them, resolving them, and approving.
// It is separate from Provider because resolving a thread has no REST
// equivalent on GitHub and no equivalent at all on some forges — a provider
// that cannot do it should fail to satisfy the interface rather than return
// "unsupported" from four methods.
type ThreadReviewer interface {
	// ReviewThreads lists the pull request's inline conversations.
	ReviewThreads(ctx context.Context, repo string, number int) ([]ReviewThread, error)
	// ReplyToThread appends a comment to the thread that inReplyTo belongs to.
	ReplyToThread(ctx context.Context, repo string, number int, inReplyTo, body string) error
	// ResolveThread marks a thread resolved.
	ResolveThread(ctx context.Context, threadID string) error
	// Approve submits an approving review on the pull request.
	Approve(ctx context.Context, repo string, number int, body string) error
}

// httpClient is the shared transport. One client, reused, keeps idle memory flat.
type httpClient struct {
	base  string
	token string
	hc    *http.Client
	// authHeader renders the provider's auth header value.
	authHeader func(token string) (name, value string)
}

func newHTTPClient(base, token string, authHeader func(string) (string, string)) *httpClient {
	return &httpClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		hc: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: refuseInsecureRedirect,
		},
		authHeader: authHeader,
	}
}

// refuseInsecureRedirect stops a redirect that would downgrade https to http.
// Every request carries a forge token, and net/http only strips sensitive
// headers when the redirect crosses to a different domain — a same-host
// https-to-http hop keeps the Authorization header and would put the token on
// the wire in cleartext.
func refuseInsecureRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}

	if req.URL.Scheme == "https" {
		return nil
	}

	for _, prev := range via {
		if prev.URL.Scheme == "https" {
			return fmt.Errorf(
				"refusing redirect from https to %s://%s: it would send the forge token in cleartext",
				req.URL.Scheme, req.URL.Host)
		}
	}

	return nil
}

// do performs a request and returns the body, failing on any non-2xx status.
func (c *httpClient) do(
	ctx context.Context,
	method, path string,
	body io.Reader,
	headers map[string]string,
) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("building %s %s: %w", method, path, err)
	}

	name, value := c.authHeader(c.token)
	req.Header.Set(name, value)

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()

	// Cap the read: a diff on a huge PR must not blow up a 4 GB box. Read one
	// byte past the cap so truncation can be detected and failed loudly
	// instead of silently reviewing a partial diff.
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s %s body: %w", method, path, err)
	}

	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("%s %s: response exceeds %d byte limit", method, path, maxResponseBytes)
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return raw, &StatusError{Method: method, Path: path, Code: res.StatusCode, Body: snippet(raw)}
	}

	return raw, nil
}

// maxResponseBytes bounds any single API response, diffs included.
const maxResponseBytes = 8 << 20

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}

	return s
}
