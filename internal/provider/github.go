package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// GitHub talks to the GitHub REST API v3.
type GitHub struct {
	c *httpClient
}

// NewGitHub returns a GitHub provider. baseURL is the API root, so GitHub
// Enterprise works by pointing it at https://host/api/v3.
func NewGitHub(baseURL, token string) *GitHub {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}

	return &GitHub{
		c: newHTTPClient(baseURL, token, func(t string) (string, string) {
			return "Authorization", "Bearer " + t
		}),
	}
}

// Name implements Provider.
func (g *GitHub) Name() string { return "github" }

const githubJSON = "application/vnd.github+json"

func githubHeaders(accept string) map[string]string {
	return map[string]string{
		"Accept":               accept,
		"X-GitHub-Api-Version": "2022-11-28",
		"Content-Type":         "application/json",
	}
}

// PullRequest implements Provider.
func (g *GitHub) PullRequest(ctx context.Context, repo string, number int) (PullRequest, error) {
	raw, err := g.c.do(
		ctx,
		"GET",
		fmt.Sprintf("/repos/%s/pulls/%d", repo, number),
		nil,
		githubHeaders(githubJSON),
	)
	if err != nil {
		return PullRequest{}, err
	}

	var payload struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return PullRequest{}, fmt.Errorf("decoding github pull request: %w", err)
	}

	return PullRequest{
		Title:   payload.Title,
		Body:    payload.Body,
		HeadSHA: payload.Head.SHA,
		BaseSHA: payload.Base.SHA,
	}, nil
}

// ListOpenPullRequests implements Provider. Pages are followed to the end so a
// repository with more than one page of open PRs does not silently lose the
// tail — an unreviewed PR is exactly what the watcher exists to catch.
func (g *GitHub) ListOpenPullRequests(ctx context.Context, repo string) ([]OpenPullRequest, error) {
	var out []OpenPullRequest

	for page := 1; page <= maxListPages; page++ {
		raw, err := g.c.do(
			ctx,
			"GET",
			fmt.Sprintf("/repos/%s/pulls?state=open&per_page=100&page=%d", repo, page),
			nil,
			githubHeaders(githubJSON),
		)
		if err != nil {
			return nil, err
		}

		var payload []struct {
			Number int  `json:"number"`
			Draft  bool `json:"draft"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				SHA string `json:"sha"`
			} `json:"base"`
		}

		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("decoding github pull request list: %w", err)
		}

		for _, p := range payload {
			// A draft has not been offered for review yet; reviewing it would
			// spend the budget before the author is asking for an opinion.
			if p.Draft {
				continue
			}

			out = append(out, OpenPullRequest{
				Number:  p.Number,
				HeadSHA: p.Head.SHA,
				BaseSHA: p.Base.SHA,
			})
		}

		if len(payload) < 100 {
			return out, nil
		}
	}

	// The last allowed page was full, so there are more open pull requests
	// than the cap admits. Returning the truncated list would leave the
	// watcher permanently blind to everything past it, with no signal.
	return nil, fmt.Errorf("%w: open pull requests on %s", ErrTooManyResults, repo)
}

// maxListPages bounds pagination so a pathological repository cannot make one
// watcher tick run unbounded against the API.
const maxListPages = 10

// Diff implements Provider.
func (g *GitHub) Diff(ctx context.Context, repo string, number int) (string, error) {
	raw, err := g.c.do(
		ctx,
		"GET",
		fmt.Sprintf("/repos/%s/pulls/%d", repo, number),
		nil,
		githubHeaders("application/vnd.github.v3.diff"),
	)
	if err != nil {
		return "", err
	}

	return string(raw), nil
}

// CompareDiff implements Provider.
func (g *GitHub) CompareDiff(ctx context.Context, repo, from, to string) (string, error) {
	raw, err := g.c.do(
		ctx,
		"GET",
		fmt.Sprintf("/repos/%s/compare/%s...%s", repo, url.PathEscape(from), url.PathEscape(to)),
		nil,
		githubHeaders("application/vnd.github.v3.diff"),
	)
	if err != nil {
		return "", err
	}

	return string(raw), nil
}

// PostInline implements Provider. The comment is anchored to the RIGHT side so
// it lands on the line the PR adds, matching what the engine was asked to cite.
func (g *GitHub) PostInline(
	ctx context.Context,
	repo string,
	number int,
	headSHA string,
	c InlineComment,
) error {
	payload, err := json.Marshal(map[string]any{
		"body":      c.Body,
		"commit_id": headSHA,
		"path":      c.Path,
		"line":      c.Line,
		"side":      "RIGHT",
	})
	if err != nil {
		return fmt.Errorf("encoding github inline comment: %w", err)
	}

	if _, err := g.c.do(
		ctx,
		"POST",
		fmt.Sprintf("/repos/%s/pulls/%d/comments", repo, number),
		bytes.NewReader(payload),
		githubHeaders(githubJSON),
	); err != nil {
		return err
	}

	return nil
}

// PostSummary implements Provider.
func (g *GitHub) PostSummary(ctx context.Context, repo string, number int, body string) (string, error) {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return "", fmt.Errorf("encoding github summary: %w", err)
	}

	raw, err := g.c.do(
		ctx,
		"POST",
		fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number),
		bytes.NewReader(payload),
		githubHeaders(githubJSON),
	)
	if err != nil {
		return "", err
	}

	var created struct {
		ID int64 `json:"id"`
	}

	if err := json.Unmarshal(raw, &created); err != nil {
		return "", fmt.Errorf("decoding github summary response: %w", err)
	}

	return fmt.Sprintf("%d", created.ID), nil
}

// UpdateSummary implements Provider.
func (g *GitHub) UpdateSummary(ctx context.Context, repo, commentID, body string) error {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return fmt.Errorf("encoding github summary update: %w", err)
	}

	if _, err := g.c.do(
		ctx,
		"PATCH",
		fmt.Sprintf("/repos/%s/issues/comments/%s", repo, strings.TrimSpace(commentID)),
		bytes.NewReader(payload),
		githubHeaders(githubJSON),
	); err != nil {
		return err
	}

	return nil
}
