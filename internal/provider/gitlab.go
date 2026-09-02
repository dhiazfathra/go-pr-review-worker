package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// GitLab talks to the GitLab REST API v4. Merge requests are addressed by
// project path (URL-encoded) and iid, matching the webhook payload.
type GitLab struct {
	c *httpClient

	// refs memoises the diff-refs triple for the revision currently being
	// commented on. Every inline comment needs it, and without the memo a pass
	// with N findings makes N extra metadata calls for an answer that cannot
	// change mid-pass. It is a single slot rather than a map because the worker
	// posts for one revision at a time, which also makes it impossible for the
	// memo to grow over the life of the process.
	refsMu  sync.Mutex
	refsKey string
	refs    gitlabDiffRefs
}

// NewGitLab returns a GitLab provider. baseURL is the API root, e.g.
// https://gitlab.com/api/v4 — self-managed instances only change the host.
func NewGitLab(baseURL, token string) *GitLab {
	if baseURL == "" {
		baseURL = "https://gitlab.com/api/v4"
	}

	return &GitLab{
		c: newHTTPClient(baseURL, token, func(t string) (string, string) {
			return "PRIVATE-TOKEN", t
		}),
	}
}

// Name implements Provider.
func (g *GitLab) Name() string { return "gitlab" }

var gitlabHeaders = map[string]string{"Content-Type": "application/json"}

func project(repo string) string {
	return url.PathEscape(repo)
}

type gitlabDiffRefs struct {
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	StartSHA string `json:"start_sha"`
}

type gitlabMR struct {
	Title       string         `json:"title"`
	Description string         `json:"description"`
	SHA         string         `json:"sha"`
	DiffRefs    gitlabDiffRefs `json:"diff_refs"`
}

func (g *GitLab) mergeRequest(ctx context.Context, repo string, iid int) (gitlabMR, error) {
	var mr gitlabMR

	raw, err := g.c.do(
		ctx,
		"GET",
		fmt.Sprintf("/projects/%s/merge_requests/%d", project(repo), iid),
		nil,
		gitlabHeaders,
	)
	if err != nil {
		return mr, err
	}

	if err := json.Unmarshal(raw, &mr); err != nil {
		return mr, fmt.Errorf("decoding gitlab merge request: %w", err)
	}

	return mr, nil
}

// ListOpenPullRequests implements Provider.
func (g *GitLab) ListOpenPullRequests(ctx context.Context, repo string) ([]OpenPullRequest, error) {
	var out []OpenPullRequest

	for page := 1; page <= maxListPages; page++ {
		raw, err := g.c.do(
			ctx,
			"GET",
			fmt.Sprintf(
				"/projects/%s/merge_requests?state=opened&per_page=100&page=%d",
				project(repo), page,
			),
			nil,
			gitlabHeaders,
		)
		if err != nil {
			return nil, err
		}

		var payload []struct {
			IID      int            `json:"iid"`
			Draft    bool           `json:"draft"`
			WIP      bool           `json:"work_in_progress"`
			SHA      string         `json:"sha"`
			DiffRefs gitlabDiffRefs `json:"diff_refs"`
		}

		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("decoding gitlab merge request list: %w", err)
		}

		for _, mr := range payload {
			if mr.Draft || mr.WIP {
				continue
			}

			head := mr.DiffRefs.HeadSHA
			if head == "" {
				head = mr.SHA
			}

			out = append(out, OpenPullRequest{
				Number:  mr.IID,
				HeadSHA: head,
				BaseSHA: mr.DiffRefs.BaseSHA,
			})
		}

		if len(payload) < 100 {
			return out, nil
		}
	}

	// See the GitHub implementation: a full last page means the cap hid open
	// merge requests, and reporting the list as complete would hide them for
	// good.
	return nil, fmt.Errorf("%w: open merge requests on %s", ErrTooManyResults, repo)
}

// PullRequest implements Provider.
func (g *GitLab) PullRequest(ctx context.Context, repo string, number int) (PullRequest, error) {
	mr, err := g.mergeRequest(ctx, repo, number)
	if err != nil {
		return PullRequest{}, err
	}

	return PullRequest{
		Title:   mr.Title,
		Body:    mr.Description,
		HeadSHA: mr.DiffRefs.HeadSHA,
		BaseSHA: mr.DiffRefs.BaseSHA,
	}, nil
}

// gitlabChange is one file entry in a changes/diffs response.
type gitlabChange struct {
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path"`
	Diff    string `json:"diff"`
}

// unified renders GitLab's per-file diff fragments as a single unified diff,
// which is the only format the review prompt accepts.
func unified(changes []gitlabChange) string {
	var b strings.Builder

	for _, ch := range changes {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", ch.OldPath, ch.NewPath)
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", ch.OldPath, ch.NewPath)
		b.WriteString(strings.TrimRight(ch.Diff, "\n"))
		b.WriteString("\n")
	}

	return b.String()
}

// Diff implements Provider.
func (g *GitLab) Diff(ctx context.Context, repo string, number int) (string, error) {
	raw, err := g.c.do(
		ctx,
		"GET",
		fmt.Sprintf("/projects/%s/merge_requests/%d/changes", project(repo), number),
		nil,
		gitlabHeaders,
	)
	if err != nil {
		return "", err
	}

	var payload struct {
		Changes []gitlabChange `json:"changes"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("decoding gitlab changes: %w", err)
	}

	return unified(payload.Changes), nil
}

// CompareDiff implements Provider.
func (g *GitLab) CompareDiff(ctx context.Context, repo, from, to string) (string, error) {
	raw, err := g.c.do(
		ctx,
		"GET",
		fmt.Sprintf(
			"/projects/%s/repository/compare?from=%s&to=%s",
			project(repo),
			url.QueryEscape(from),
			url.QueryEscape(to),
		),
		nil,
		gitlabHeaders,
	)
	if err != nil {
		return "", err
	}

	var payload struct {
		Diffs []gitlabChange `json:"diffs"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("decoding gitlab compare: %w", err)
	}

	return unified(payload.Diffs), nil
}

// diffRefs returns the position triple for a merge request at headSHA, reading
// it from the API at most once per revision.
func (g *GitLab) diffRefs(ctx context.Context, repo string, iid int, headSHA string) (gitlabDiffRefs, error) {
	key := fmt.Sprintf("%s#%d@%s", repo, iid, headSHA)

	g.refsMu.Lock()
	cached, hit := g.refs, g.refsKey == key
	g.refsMu.Unlock()

	if hit {
		return cached, nil
	}

	mr, err := g.mergeRequest(ctx, repo, iid)
	if err != nil {
		return gitlabDiffRefs{}, err
	}

	g.refsMu.Lock()
	g.refsKey, g.refs = key, mr.DiffRefs
	g.refsMu.Unlock()

	return mr.DiffRefs, nil
}

// PostInline implements Provider. GitLab needs the full diff refs triple to
// anchor a discussion; they are fetched once per revision and cached.
func (g *GitLab) PostInline(
	ctx context.Context,
	repo string,
	number int,
	headSHA string,
	c InlineComment,
) error {
	refs, err := g.diffRefs(ctx, repo, number, headSHA)
	if err != nil {
		return err
	}

	form := url.Values{}
	form.Set("body", c.Body)
	form.Set("position[position_type]", "text")
	form.Set("position[base_sha]", refs.BaseSHA)
	form.Set("position[start_sha]", refs.StartSHA)
	form.Set("position[head_sha]", headSHA)
	form.Set("position[new_path]", c.Path)
	form.Set("position[old_path]", c.Path)
	form.Set("position[new_line]", fmt.Sprintf("%d", c.Line))

	if _, err := g.c.do(
		ctx,
		"POST",
		fmt.Sprintf("/projects/%s/merge_requests/%d/discussions", project(repo), number),
		strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	); err != nil {
		return err
	}

	return nil
}

// PostSummary implements Provider.
func (g *GitLab) PostSummary(ctx context.Context, repo string, number int, body string) (string, error) {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return "", fmt.Errorf("encoding gitlab note: %w", err)
	}

	raw, err := g.c.do(
		ctx,
		"POST",
		fmt.Sprintf("/projects/%s/merge_requests/%d/notes", project(repo), number),
		bytes.NewReader(payload),
		gitlabHeaders,
	)
	if err != nil {
		return "", err
	}

	var created struct {
		ID int64 `json:"id"`
	}

	if err := json.Unmarshal(raw, &created); err != nil {
		return "", fmt.Errorf("decoding gitlab note response: %w", err)
	}

	// The note id alone cannot address the note later; the MR iid is part of
	// the update path, so both travel in the stored id.
	return fmt.Sprintf("%d/%d", number, created.ID), nil
}

// UpdateSummary implements Provider. commentID is "<mr iid>/<note id>" as
// returned by PostSummary.
func (g *GitLab) UpdateSummary(ctx context.Context, repo, commentID, body string) error {
	iid, noteID, ok := strings.Cut(commentID, "/")
	if !ok {
		return fmt.Errorf("malformed gitlab comment id %q, want \"<iid>/<note>\"", commentID)
	}

	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return fmt.Errorf("encoding gitlab note update: %w", err)
	}

	if _, err := g.c.do(
		ctx,
		"PUT",
		fmt.Sprintf("/projects/%s/merge_requests/%s/notes/%s", project(repo), iid, noteID),
		bytes.NewReader(payload),
		gitlabHeaders,
	); err != nil {
		return err
	}

	return nil
}
