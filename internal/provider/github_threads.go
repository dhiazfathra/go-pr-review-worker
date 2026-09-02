package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// GitHub implements ThreadReviewer: the follow-up pass reads its own threads,
// answers them, resolves the fixed ones, and approves when nothing is left.
var _ ThreadReviewer = (*GitHub)(nil)

// reviewThreadsQuery pages through a pull request's inline conversations.
// Resolving a thread is a GraphQL-only mutation on GitHub, and the thread's
// node id comes back only from GraphQL, so reading the threads has to happen
// here too rather than over REST.
// `viewer` is the account the token belongs to, asked for in the same round
// trip: thread ownership is decided by who wrote the first comment, not by a
// marker in its body, which anyone can paste.
//
// The comments come back as two windows rather than one `comments(first:100)`.
// A connection returns at most 100 nodes per request, so a single window loses
// whichever end it is not anchored to — and both ends matter here: the first
// comment is the finding, and the newest ones are the author's replies, which
// are the evidence. `opening` pins the finding, `latest` pins the replies. A
// thread over 101 comments long loses its middle, which no verdict depends on.
const reviewThreadsQuery = `
query($owner:String!,$name:String!,$number:Int!,$cursor:String) {
  viewer { login }
  repository(owner:$owner,name:$name) {
    pullRequest(number:$number) {
      reviewThreads(first:100,after:$cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          path
          line
          originalLine
          isResolved
          opening: comments(first:1) {
            nodes { databaseId author { login } body }
          }
          latest: comments(last:100) {
            nodes { databaseId author { login } body }
          }
        }
      }
    }
  }
}`

const resolveThreadMutation = `
mutation($id:ID!) {
  resolveReviewThread(input:{threadId:$id}) { thread { id isResolved } }
}`

// graphql posts one GraphQL document and unmarshals data into out. GitHub
// answers a failed query with HTTP 200 and an "errors" array, so the status
// code alone would report a broken query as a success with empty data.
func (g *GitHub) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("encoding github graphql request: %w", err)
	}

	raw, err := g.c.do(ctx, "POST", "/graphql", bytes.NewReader(payload), map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
	})
	if err != nil {
		return err
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decoding github graphql response: %w", err)
	}

	if len(envelope.Errors) > 0 {
		messages := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			messages = append(messages, e.Message)
		}

		return fmt.Errorf("github graphql: %s", strings.Join(messages, "; "))
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decoding github graphql data: %w", err)
	}

	return nil
}

// splitRepo divides "owner/name" for the GraphQL API, which takes them apart
// rather than as one path.
func splitRepo(repo string) (string, string, error) {
	owner, name, ok := strings.Cut(strings.Trim(repo, "/"), "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("repository %q is not owner/name", repo)
	}

	return owner, name, nil
}

// ReviewThreads implements ThreadReviewer.
func (g *GitHub) ReviewThreads(ctx context.Context, repo string, number int) ([]ReviewThread, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}

	var (
		out    []ReviewThread
		cursor *string
	)

	type commentWindow struct {
		Nodes []struct {
			DatabaseID int64 `json:"databaseId"`
			Author     struct {
				Login string `json:"login"`
			} `json:"author"`
			Body string `json:"body"`
		} `json:"nodes"`
	}

	for page := 0; page < maxListPages; page++ {
		var res struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID           string        `json:"id"`
							Path         string        `json:"path"`
							Line         *int          `json:"line"`
							OriginalLine *int          `json:"originalLine"`
							IsResolved   bool          `json:"isResolved"`
							Opening      commentWindow `json:"opening"`
							Latest       commentWindow `json:"latest"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}

		vars := map[string]any{"owner": owner, "name": name, "number": number}
		if cursor != nil {
			vars["cursor"] = *cursor
		}

		if err := g.graphql(ctx, reviewThreadsQuery, vars, &res); err != nil {
			return nil, err
		}

		threads := res.Repository.PullRequest.ReviewThreads

		for _, n := range threads.Nodes {
			t := ReviewThread{
				ID:       n.ID,
				Path:     n.Path,
				Resolved: n.IsResolved,
			}

			// line is null once the thread's anchor scrolls out of the current
			// diff (an outdated thread); originalLine still says where it was,
			// and reporting nothing there would lose the citation entirely.
			switch {
			case n.Line != nil:
				t.Line = *n.Line
			case n.OriginalLine != nil:
				t.Line = *n.OriginalLine
			}

			seen := make(map[int64]bool, len(n.Latest.Nodes)+1)

			for _, w := range []commentWindow{n.Opening, n.Latest} {
				for _, c := range w.Nodes {
					// The windows overlap on short threads, where `last:100`
					// includes the opening comment.
					if seen[c.DatabaseID] {
						continue
					}

					seen[c.DatabaseID] = true

					t.Comments = append(t.Comments, ThreadComment{
						ID:     fmt.Sprintf("%d", c.DatabaseID),
						Author: c.Author.Login,
						Body:   c.Body,
					})
				}
			}

			if len(t.Comments) > 0 && res.Viewer.Login != "" {
				t.StartedByWorker = t.Comments[0].Author == res.Viewer.Login
			}

			out = append(out, t)
		}

		if !threads.PageInfo.HasNextPage {
			return out, nil
		}

		next := threads.PageInfo.EndCursor
		cursor = &next
	}

	// Falling out of the loop means the forge still had pages. A truncated
	// thread list would let the follow-up pass approve a pull request while
	// unread findings sat on a page it never asked for.
	return nil, fmt.Errorf("%w: review threads on %s#%d", ErrTooManyResults, repo, number)
}

// ReplyToThread implements ThreadReviewer. GitHub's REST reply endpoint takes
// the id of a comment already in the thread, which is why the caller passes a
// comment id rather than the thread's node id.
func (g *GitHub) ReplyToThread(ctx context.Context, repo string, number int, inReplyTo, body string) error {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return fmt.Errorf("encoding github thread reply: %w", err)
	}

	if _, err := g.c.do(
		ctx,
		"POST",
		fmt.Sprintf("/repos/%s/pulls/%d/comments/%s/replies", repo, number, strings.TrimSpace(inReplyTo)),
		bytes.NewReader(payload),
		githubHeaders(githubJSON),
	); err != nil {
		return err
	}

	return nil
}

// ResolveThread implements ThreadReviewer.
func (g *GitHub) ResolveThread(ctx context.Context, threadID string) error {
	return g.graphql(ctx, resolveThreadMutation, map[string]any{"id": threadID}, nil)
}

// Approve implements ThreadReviewer.
func (g *GitHub) Approve(ctx context.Context, repo string, number int, body string) error {
	payload, err := json.Marshal(map[string]string{"body": body, "event": "APPROVE"})
	if err != nil {
		return fmt.Errorf("encoding github approval: %w", err)
	}

	if _, err := g.c.do(
		ctx,
		"POST",
		fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number),
		bytes.NewReader(payload),
		githubHeaders(githubJSON),
	); err != nil {
		return err
	}

	return nil
}
