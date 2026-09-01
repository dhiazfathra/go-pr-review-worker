package provider_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dhiazfathra/go-pr-review-worker/internal/provider"
)

type recorded struct {
	method string
	path   string
	query  string
	accept string
	auth   string
	body   string
}

// server returns a stub forge that records requests and replies from a routing
// table keyed by "METHOD /path".
func server(t *testing.T, routes map[string]string) (*httptest.Server, *[]recorded) {
	t.Helper()

	var log []recorded

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		log = append(log, recorded{
			method: r.Method,
			path:   r.URL.EscapedPath(),
			query:  r.URL.RawQuery,
			accept: r.Header.Get("Accept"),
			auth:   r.Header.Get("Authorization") + r.Header.Get("PRIVATE-TOKEN"),
			body:   string(body),
		})

		// EscapedPath keeps GitLab's %2F-encoded project path intact; the
		// decoded form would collapse it into extra path segments.
		res, ok := routes[r.Method+" "+r.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))

			return
		}

		_, _ = w.Write([]byte(res))
	}))

	t.Cleanup(srv.Close)

	return srv, &log
}

func TestGitHubPullRequestAndDiff(t *testing.T) {
	srv, log := server(t, map[string]string{
		"GET /repos/acme/app/pulls/7": `{"title":"Add cache","body":"desc",
			"head":{"sha":"h1"},"base":{"sha":"b1"}}`,
	})

	gh := provider.NewGitHub(srv.URL, "tok")

	pr, err := gh.PullRequest(context.Background(), "acme/app", 7)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}

	if pr.Title != "Add cache" || pr.HeadSHA != "h1" || pr.BaseSHA != "b1" {
		t.Fatalf("pr = %+v", pr)
	}

	if got := (*log)[0].auth; got != "Bearer tok" {
		t.Fatalf("auth header = %q", got)
	}

	if _, err := gh.Diff(context.Background(), "acme/app", 7); err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if got := (*log)[1].accept; got != "application/vnd.github.v3.diff" {
		t.Fatalf("diff accept header = %q; a JSON body would not parse as a diff", got)
	}

	if gh.Name() != "github" {
		t.Fatalf("Name = %q", gh.Name())
	}
}

func TestGitHubCompareDiffUsesThreeDotRange(t *testing.T) {
	srv, log := server(t, map[string]string{
		"GET /repos/acme/app/compare/sha1...sha2": "@@ delta @@",
	})

	diff, err := provider.NewGitHub(srv.URL, "tok").
		CompareDiff(context.Background(), "acme/app", "sha1", "sha2")
	if err != nil {
		t.Fatalf("CompareDiff: %v", err)
	}

	if diff != "@@ delta @@" {
		t.Fatalf("diff = %q", diff)
	}

	if !strings.Contains((*log)[0].path, "sha1...sha2") {
		t.Fatalf("path = %q", (*log)[0].path)
	}
}

func TestGitHubPostsInlineOnTheRightSide(t *testing.T) {
	srv, log := server(t, map[string]string{
		"POST /repos/acme/app/pulls/7/comments": `{"id":1}`,
	})

	err := provider.NewGitHub(srv.URL, "tok").PostInline(
		context.Background(),
		"acme/app",
		7,
		"h1",
		provider.InlineComment{Path: "a.go", Line: 12, Body: "problem"},
	)
	if err != nil {
		t.Fatalf("PostInline: %v", err)
	}

	body := (*log)[0].body

	for _, want := range []string{`"side":"RIGHT"`, `"commit_id":"h1"`, `"line":12`, `"path":"a.go"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
}

func TestGitHubSummaryPostThenUpdate(t *testing.T) {
	srv, log := server(t, map[string]string{
		"POST /repos/acme/app/issues/7/comments":   `{"id":99}`,
		"PATCH /repos/acme/app/issues/comments/99": `{"id":99}`,
	})

	gh := provider.NewGitHub(srv.URL, "tok")

	id, err := gh.PostSummary(context.Background(), "acme/app", 7, "hello")
	if err != nil {
		t.Fatalf("PostSummary: %v", err)
	}

	if id != "99" {
		t.Fatalf("id = %q, want 99", id)
	}

	if err := gh.UpdateSummary(context.Background(), "acme/app", id, "edited"); err != nil {
		t.Fatalf("UpdateSummary: %v", err)
	}

	if (*log)[1].method != "PATCH" {
		t.Fatalf("update used %s, want PATCH", (*log)[1].method)
	}
}

func TestGitHubErrorsCarryStatusAndBody(t *testing.T) {
	srv, _ := server(t, map[string]string{})
	gh := provider.NewGitHub(srv.URL, "tok")

	_, err := gh.PullRequest(context.Background(), "acme/app", 7)
	if err == nil {
		t.Fatal("want error for 404")
	}

	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want status and body detail", err)
	}

	if _, err := gh.Diff(context.Background(), "acme/app", 7); err == nil {
		t.Fatal("Diff: want error")
	}

	if _, err := gh.CompareDiff(context.Background(), "acme/app", "a", "b"); err == nil {
		t.Fatal("CompareDiff: want error")
	}

	if err := gh.PostInline(context.Background(), "acme/app", 7, "h", provider.InlineComment{}); err == nil {
		t.Fatal("PostInline: want error")
	}

	if _, err := gh.PostSummary(context.Background(), "acme/app", 7, "x"); err == nil {
		t.Fatal("PostSummary: want error")
	}

	if err := gh.UpdateSummary(context.Background(), "acme/app", "1", "x"); err == nil {
		t.Fatal("UpdateSummary: want error")
	}
}

func TestGitHubMalformedJSONIsReported(t *testing.T) {
	srv, _ := server(t, map[string]string{
		"GET /repos/acme/app/pulls/7":            `{"title":`,
		"POST /repos/acme/app/issues/7/comments": `not json`,
	})

	gh := provider.NewGitHub(srv.URL, "tok")

	if _, err := gh.PullRequest(context.Background(), "acme/app", 7); err == nil {
		t.Fatal("want decode error")
	}

	if _, err := gh.PostSummary(context.Background(), "acme/app", 7, "x"); err == nil {
		t.Fatal("want decode error")
	}
}

func TestGitHubDefaultBaseURL(t *testing.T) {
	// No network call: only prove the default is wired, since an empty base
	// would otherwise produce a relative, unusable URL.
	if _, err := provider.NewGitHub("", "tok").
		PullRequest(t.Context(), "acme/app", 7); err == nil {
		t.Skip("unexpectedly reached api.github.com; nothing to assert offline")
	}
}

func TestGitLabDiffRendersUnifiedDiff(t *testing.T) {
	srv, log := server(t, map[string]string{
		"GET /projects/grp%2Fproj/merge_requests/4/changes": `{"changes":[
			{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\n-old\n+new\n"}]}`,
	})

	gl := provider.NewGitLab(srv.URL, "tok")

	diff, err := gl.Diff(context.Background(), "grp/proj", 4)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	for _, want := range []string{"diff --git a/a.go b/a.go", "--- a/a.go", "+++ b/a.go", "+new"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, diff)
		}
	}

	if got := (*log)[0].auth; got != "tok" {
		t.Fatalf("PRIVATE-TOKEN = %q", got)
	}

	if gl.Name() != "gitlab" {
		t.Fatalf("Name = %q", gl.Name())
	}
}

func TestGitLabPullRequestAndCompare(t *testing.T) {
	srv, log := server(t, map[string]string{
		"GET /projects/grp%2Fproj/merge_requests/4": `{"title":"t","description":"d",
			"sha":"h1","diff_refs":{"base_sha":"b1","head_sha":"h1","start_sha":"s1"}}`,
		"GET /projects/grp%2Fproj/repository/compare": `{"diffs":[
			{"old_path":"a.go","new_path":"a.go","diff":"@@ x @@"}]}`,
	})

	gl := provider.NewGitLab(srv.URL, "tok")

	pr, err := gl.PullRequest(context.Background(), "grp/proj", 4)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}

	if pr.HeadSHA != "h1" || pr.BaseSHA != "b1" || pr.Title != "t" {
		t.Fatalf("pr = %+v", pr)
	}

	if _, err := gl.CompareDiff(context.Background(), "grp/proj", "a", "b"); err != nil {
		t.Fatalf("CompareDiff: %v", err)
	}

	if q := (*log)[1].query; q != "from=a&to=b" {
		t.Fatalf("query = %q", q)
	}
}

func TestGitLabInlineUsesFullPositionTriple(t *testing.T) {
	srv, log := server(t, map[string]string{
		"GET /projects/grp%2Fproj/merge_requests/4": `{"diff_refs":
			{"base_sha":"b1","head_sha":"h1","start_sha":"s1"}}`,
		"POST /projects/grp%2Fproj/merge_requests/4/discussions": `{"id":"x"}`,
	})

	err := provider.NewGitLab(srv.URL, "tok").PostInline(
		context.Background(),
		"grp/proj",
		4,
		"h2",
		provider.InlineComment{Path: "a.go", Line: 5, Body: "problem"},
	)
	if err != nil {
		t.Fatalf("PostInline: %v", err)
	}

	body := (*log)[1].body

	for _, want := range []string{
		"position%5Bbase_sha%5D=b1",
		"position%5Bstart_sha%5D=s1",
		"position%5Bhead_sha%5D=h2",
		"position%5Bnew_line%5D=5",
		"position%5Bposition_type%5D=text",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("form missing %s: %s", want, body)
		}
	}
}

func TestGitLabSummaryIDCarriesTheMRIID(t *testing.T) {
	srv, log := server(t, map[string]string{
		"POST /projects/grp%2Fproj/merge_requests/4/notes":   `{"id":77}`,
		"PUT /projects/grp%2Fproj/merge_requests/4/notes/77": `{"id":77}`,
	})

	gl := provider.NewGitLab(srv.URL, "tok")

	id, err := gl.PostSummary(context.Background(), "grp/proj", 4, "hello")
	if err != nil {
		t.Fatalf("PostSummary: %v", err)
	}

	if id != "4/77" {
		t.Fatalf("id = %q, want 4/77 so the note can be edited later", id)
	}

	if err := gl.UpdateSummary(context.Background(), "grp/proj", id, "edited"); err != nil {
		t.Fatalf("UpdateSummary: %v", err)
	}

	if (*log)[1].method != "PUT" {
		t.Fatalf("update used %s, want PUT", (*log)[1].method)
	}
}

func TestGitLabUpdateSummaryRejectsMalformedID(t *testing.T) {
	srv, _ := server(t, map[string]string{})

	err := provider.NewGitLab(srv.URL, "tok").
		UpdateSummary(context.Background(), "grp/proj", "77", "x")
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("err = %v, want a malformed-id error", err)
	}
}

func TestGitLabErrorPaths(t *testing.T) {
	srv, _ := server(t, map[string]string{
		"GET /projects/grp%2Fproj/merge_requests/4/changes": `{"changes":`,
	})

	gl := provider.NewGitLab(srv.URL, "tok")

	if _, err := gl.Diff(context.Background(), "grp/proj", 4); err == nil {
		t.Fatal("Diff: want decode error")
	}

	if _, err := gl.PullRequest(context.Background(), "grp/proj", 4); err == nil {
		t.Fatal("PullRequest: want error")
	}

	if _, err := gl.CompareDiff(context.Background(), "grp/proj", "a", "b"); err == nil {
		t.Fatal("CompareDiff: want error")
	}

	if err := gl.PostInline(context.Background(), "grp/proj", 4, "h", provider.InlineComment{}); err == nil {
		t.Fatal("PostInline: want error")
	}

	if _, err := gl.PostSummary(context.Background(), "grp/proj", 4, "x"); err == nil {
		t.Fatal("PostSummary: want error")
	}

	if err := gl.UpdateSummary(context.Background(), "grp/proj", "4/1", "x"); err == nil {
		t.Fatal("UpdateSummary: want error")
	}
}

func TestUnreachableHostIsReported(t *testing.T) {
	gh := provider.NewGitHub("http://127.0.0.1:1", "tok")
	if _, err := gh.PullRequest(context.Background(), "acme/app", 1); err == nil {
		t.Fatal("want transport error")
	}

	gl := provider.NewGitLab("http://127.0.0.1:1", "tok")
	if _, err := gl.PullRequest(context.Background(), "grp/proj", 1); err == nil {
		t.Fatal("want transport error")
	}
}

func TestInvalidMethodOrURLIsReported(t *testing.T) {
	// A control character in the repo name makes the request unbuildable,
	// which must surface as an error rather than a panic.
	gh := provider.NewGitHub("http://example.invalid", "tok")
	if _, err := gh.PullRequest(context.Background(), "acme/\x7f", 1); err == nil {
		t.Fatal("want request build error")
	}
}

func TestGitLabInlineFetchesDiffRefsOncePerRevision(t *testing.T) {
	// Review feedback (PR #1): the merge request was re-read for every finding,
	// multiplying API calls and rate-limit exposure by the comment count.
	srv, log := server(t, map[string]string{
		"GET /projects/grp%2Fproj/merge_requests/4": `{"diff_refs":
			{"base_sha":"b1","head_sha":"h1","start_sha":"s1"}}`,
		"POST /projects/grp%2Fproj/merge_requests/4/discussions": `{"id":"x"}`,
	})

	gl := provider.NewGitLab(srv.URL, "tok")

	for i := range 3 {
		err := gl.PostInline(context.Background(), "grp/proj", 4, "h2", provider.InlineComment{
			Path: "a.go",
			Line: i + 1,
			Body: "problem",
		})
		if err != nil {
			t.Fatalf("PostInline %d: %v", i, err)
		}
	}

	gets := 0

	for _, r := range *log {
		if r.method == "GET" {
			gets++
		}
	}

	if gets != 1 {
		t.Fatalf("merge request fetched %d times for 3 comments, want 1", gets)
	}

	// A new revision must invalidate the memo, or comments would be anchored
	// against stale refs after a push.
	if err := gl.PostInline(context.Background(), "grp/proj", 4, "h3", provider.InlineComment{
		Path: "a.go",
		Line: 1,
		Body: "problem",
	}); err != nil {
		t.Fatalf("PostInline after push: %v", err)
	}

	gets = 0

	for _, r := range *log {
		if r.method == "GET" {
			gets++
		}
	}

	if gets != 2 {
		t.Fatalf("GETs = %d after a new head sha, want 2", gets)
	}
}
