package review

import (
	"context"
	"fmt"
	"strings"
)

// Thread resolution is exposed by GraphQL, including outdated review threads.
// REST review comments do not tell us whether their conversations are resolved.
func (g *GitHub) conversationsResolved(ctx context.Context, number int) (bool, error) {
	owner, repo, ok := strings.Cut(g.p.Repo, "/")
	if !ok {
		return false, fmt.Errorf("github: invalid repository")
	}
	endpoint := strings.TrimRight(g.p.APIURL, "/")
	if strings.HasSuffix(endpoint, "/api/v3") {
		endpoint = strings.TrimSuffix(endpoint, "/api/v3") + "/api"
	}
	endpoint += "/graphql"
	const query = `query($owner:String!,$repo:String!,$number:Int!,$cursor:String) {
  repository(owner:$owner,name:$repo) {
    pullRequest(number:$number) {
      reviewThreads(first:100,after:$cursor) {
        nodes { isResolved }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`
	var cursor *string
	for page := 0; page < 1000; page++ {
		var result struct {
			Errors []struct{ Message string }
			Data   struct {
				Repository *struct {
					PullRequest *struct {
						ReviewThreads *struct {
							Nodes    []struct{ IsResolved *bool }
							PageInfo *struct {
								HasNextPage *bool
								EndCursor   string
							}
						}
					}
				}
			}
		}
		input := map[string]any{"query": query, "variables": map[string]any{"owner": owner, "repo": repo, "number": number, "cursor": cursor}}
		if err := g.call(ctx, "POST", endpoint, input, &result); err != nil {
			return false, err
		}
		if len(result.Errors) != 0 || result.Data.Repository == nil || result.Data.Repository.PullRequest == nil || result.Data.Repository.PullRequest.ReviewThreads == nil {
			return false, fmt.Errorf("github: review conversations unavailable for PR #%d", number)
		}
		threads := result.Data.Repository.PullRequest.ReviewThreads
		if threads.Nodes == nil || threads.PageInfo == nil || threads.PageInfo.HasNextPage == nil {
			return false, fmt.Errorf("github: missing review-thread pagination")
		}
		for _, thread := range threads.Nodes {
			if thread.IsResolved == nil {
				return false, fmt.Errorf("github: missing review-thread resolution")
			}
			if !*thread.IsResolved {
				return false, nil
			}
		}
		if !*threads.PageInfo.HasNextPage {
			return true, nil
		}
		if threads.PageInfo.EndCursor == "" || (cursor != nil && *cursor == threads.PageInfo.EndCursor) {
			return false, fmt.Errorf("github: invalid review-thread cursor")
		}
		cursor = &threads.PageInfo.EndCursor
	}
	return false, fmt.Errorf("github: review-thread pagination exceeds limit")
}
