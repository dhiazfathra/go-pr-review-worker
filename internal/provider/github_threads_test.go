package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dhiazfathra/go-pr-review-worker/internal/provider"
)

func TestGitHubListOpenPullRequestsSkipsDrafts(t *testing.T) {
	srv, log := server(t, map[string]string{
		"GET /repos/o/r/pulls": `[
		  {"number":1,"draft":false,"head":{"sha":"h1"},"base":{"sha":"b1"}},
		  {"number":2,"draft":true,"head":{"sha":"h2"},"base":{"sha":"b2"}}
		]`,
	})
	defer srv.Close()

	got, err := provider.NewGitHub(srv.URL, "tok").ListOpenPullRequests(context.Background(), "o/r")
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}

	if len(got) != 1 || got[0].Number != 1 || got[0].HeadSHA != "h1" {
		t.Fatalf("got = %+v, want only the non-draft PR", got)
	}

	if !strings.Contains((*log)[0].query, "state=open") {
		t.Errorf("query = %q, want state=open", (*log)[0].query)
	}
}

func TestGitHubReviewThreadsReadsCommentsAndResolution(t *testing.T) {
	srv, _ := server(t, map[string]string{
		"POST /graphql": `{"data":{"viewer":{"login":"bot"},"repository":{"pullRequest":{"reviewThreads":{
		  "pageInfo":{"hasNextPage":false,"endCursor":""},
		  "nodes":[
		    {"id":"T1","path":"a.ts","line":30,"originalLine":28,"isResolved":false,
		     "opening":{"nodes":[{"databaseId":11,"author":{"login":"bot"},"body":"finding"}]},
		     "latest":{"nodes":[
		       {"databaseId":11,"author":{"login":"bot"},"body":"finding"},
		       {"databaseId":12,"author":{"login":"dev"},"body":"fixed in abc"}]}},
		    {"id":"T2","path":"b.ts","line":null,"originalLine":9,"isResolved":true,
		     "opening":{"nodes":[{"databaseId":21,"author":{"login":"dev"},"body":"other"}]},
		     "latest":{"nodes":[{"databaseId":21,"author":{"login":"dev"},"body":"other"}]}}
		  ]}}}}}`,
	})
	defer srv.Close()

	got, err := provider.NewGitHub(srv.URL, "tok").ReviewThreads(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatalf("ReviewThreads: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("threads = %d, want 2", len(got))
	}

	if got[0].ID != "T1" || got[0].Line != 30 || got[0].Resolved {
		t.Errorf("thread 1 = %+v", got[0])
	}

	if len(got[0].Comments) != 2 || got[0].Comments[1].Author != "dev" || got[0].Comments[0].ID != "11" {
		t.Errorf("thread 1 comments = %+v", got[0].Comments)
	}

	// An outdated thread reports line: null; without the originalLine fallback
	// the citation would be lost entirely.
	if got[1].Line != 9 {
		t.Errorf("outdated thread line = %d, want the originalLine fallback of 9", got[1].Line)
	}

	if !got[1].Resolved {
		t.Error("thread 2 should be reported resolved")
	}

	// The two comment windows overlap on a short thread; the opening comment
	// must appear once, and first.
	if got[0].Comments[0].Body != "finding" {
		t.Errorf("thread 1 first comment = %q, want the finding", got[0].Comments[0].Body)
	}

	// Ownership comes from viewer.login, not from anything in the body: T1's
	// opening comment is the viewer's, T2's is a human's.
	if !got[0].StartedByWorker {
		t.Error("thread 1 opened by the viewer should be marked as the worker's")
	}

	if got[1].StartedByWorker {
		t.Error("thread 2 opened by another account must not be marked as the worker's")
	}
}

// A truncated thread list would let the follow-up pass approve a PR while
// unread findings sat on a page it never fetched, so the cap is an error.
func TestGitHubReviewThreadsRefusesToTruncate(t *testing.T) {
	srv, _ := server(t, map[string]string{
		"POST /graphql": `{"data":{"viewer":{"login":"bot"},"repository":{"pullRequest":{"reviewThreads":{
		  "pageInfo":{"hasNextPage":true,"endCursor":"c1"},
		  "nodes":[]}}}}}`,
	})
	defer srv.Close()

	_, err := provider.NewGitHub(srv.URL, "tok").ReviewThreads(context.Background(), "o/r", 7)
	if !errors.Is(err, provider.ErrTooManyResults) {
		t.Fatalf("err = %v, want ErrTooManyResults", err)
	}
}

// GraphQL answers a broken query with HTTP 200 and an errors array, so the
// status code alone would read a failure as an empty success.
func TestGitHubGraphQLErrorsAreNotSilentSuccesses(t *testing.T) {
	srv, _ := server(t, map[string]string{
		"POST /graphql": `{"data":null,"errors":[{"message":"Could not resolve to a Repository"}]}`,
	})
	defer srv.Close()

	_, err := provider.NewGitHub(srv.URL, "tok").ReviewThreads(context.Background(), "o/r", 7)
	if err == nil {
		t.Fatal("err = nil, want the GraphQL errors array surfaced")
	}

	if !strings.Contains(err.Error(), "Could not resolve to a Repository") {
		t.Errorf("err = %v, want the provider's message", err)
	}
}

func TestGitHubResolveThreadPostsTheMutation(t *testing.T) {
	srv, log := server(t, map[string]string{
		"POST /graphql": `{"data":{"resolveReviewThread":{"thread":{"id":"T1","isResolved":true}}}}`,
	})
	defer srv.Close()

	if err := provider.NewGitHub(srv.URL, "tok").ResolveThread(context.Background(), "T1"); err != nil {
		t.Fatalf("ResolveThread: %v", err)
	}

	body := (*log)[0].body
	if !strings.Contains(body, "resolveReviewThread") || !strings.Contains(body, `"T1"`) {
		t.Fatalf("request body = %q, want the resolve mutation for T1", body)
	}
}

func TestGitHubReplyToThreadUsesTheRepliesEndpoint(t *testing.T) {
	srv, log := server(t, map[string]string{
		"POST /repos/o/r/pulls/7/comments/11/replies": `{"id":99}`,
	})
	defer srv.Close()

	if err := provider.NewGitHub(srv.URL, "tok").
		ReplyToThread(context.Background(), "o/r", 7, "11", "still open"); err != nil {
		t.Fatalf("ReplyToThread: %v", err)
	}

	if !strings.Contains((*log)[0].body, "still open") {
		t.Errorf("body = %q", (*log)[0].body)
	}
}

func TestGitHubApproveSubmitsAnApprovingReview(t *testing.T) {
	srv, log := server(t, map[string]string{
		"POST /repos/o/r/pulls/7/reviews": `{"id":5}`,
	})
	defer srv.Close()

	if err := provider.NewGitHub(srv.URL, "tok").
		Approve(context.Background(), "o/r", 7, "all clear"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	body := (*log)[0].body
	if !strings.Contains(body, `"event":"APPROVE"`) {
		t.Fatalf("body = %q, want an APPROVE event", body)
	}
}

func TestGitHubReviewThreadsRejectsAMalformedRepo(t *testing.T) {
	srv, _ := server(t, map[string]string{})
	defer srv.Close()

	if _, err := provider.NewGitHub(srv.URL, "tok").
		ReviewThreads(context.Background(), "notownername", 7); err == nil {
		t.Fatal("err = nil, want a rejection: GraphQL needs owner and name apart")
	}
}

func TestGitLabListOpenMergeRequestsSkipsDrafts(t *testing.T) {
	srv, log := server(t, map[string]string{
		"GET /projects/g%2Fp/merge_requests": `[
		  {"iid":4,"draft":false,"work_in_progress":false,"sha":"s4","diff_refs":{"base_sha":"b4","head_sha":"h4"}},
		  {"iid":5,"draft":true,"work_in_progress":false,"sha":"s5","diff_refs":{"base_sha":"b5","head_sha":"h5"}}
		]`,
	})
	defer srv.Close()

	got, err := provider.NewGitLab(srv.URL, "tok").ListOpenPullRequests(context.Background(), "g/p")
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}

	if len(got) != 1 || got[0].Number != 4 || got[0].HeadSHA != "h4" {
		t.Fatalf("got = %+v, want only the non-draft MR", got)
	}

	if !strings.Contains((*log)[0].query, "state=opened") {
		t.Errorf("query = %q, want state=opened", (*log)[0].query)
	}
}

// GitLab cannot resolve threads or approve through this adapter, so it must not
// satisfy ThreadReviewer — the worker's capability check is what keeps it from
// calling methods that do not exist.
func TestGitLabIsNotAThreadReviewer(t *testing.T) {
	var p any = provider.NewGitLab("https://gitlab.com/api/v4", "tok")

	if _, ok := p.(provider.ThreadReviewer); ok {
		t.Fatal("GitLab claims ThreadReviewer; the follow-up pass would call unimplemented behaviour")
	}
}
