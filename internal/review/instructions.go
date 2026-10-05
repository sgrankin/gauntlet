package review

import (
	"context"
	"fmt"
	"strings"
)

const instructionsMarker = "<!-- gauntlet:instructions -->"

// Instructions are advisory and have their own comment, independent of run feedback.
func (g *GitHub) instructions(ctx context.Context, p pull, comments []comment) error {
	g.feedbackMu.Lock()
	defer g.feedbackMu.Unlock()
	body := fmt.Sprintf(`This PR is managed by Gauntlet. To land it:

- Comment `+"`@%s check`"+` to see authorization and readiness requirements.
- Comment `+"`@%s merge`"+` to request this PR when it has no unlanded prerequisites.
- For a stack, explicitly use `+"`@%s merge stack`"+` to request the stack through this PR, `+"`@%s merge-ready`"+` for its ready prefix, or `+"`@%s merge-prefix N`"+` for the bottom N PRs.
- Comment `+"`@%s cancel`"+` to withdraw this PR's request.

Gauntlet waits for policy requirements, verifies one squashed commit per PR on the target, and pushes the exact tested commit. Success closes the PR with a commit link; failure posts details here. Use Gauntlet commands instead of GitHub's merge button.

%s`, g.p.Bot, g.p.Bot, g.p.Bot, g.p.Bot, g.p.Bot, g.p.Bot, instructionsMarker)
	for _, c := range comments {
		if !strings.Contains(c.Body, instructionsMarker) {
			continue
		}
		owned, err := g.ownsComment(ctx, c)
		if err != nil {
			return err
		}
		if !owned {
			continue
		}
		if c.Body == body {
			return nil
		}
		return g.call(ctx, "PATCH", g.endpoint(fmt.Sprintf("/issues/comments/%d", c.ID)), map[string]string{"body": body}, nil)
	}
	var created comment
	if err := g.call(ctx, "POST", g.endpoint(fmt.Sprintf("/issues/%d/comments", p.Number)), map[string]string{"body": body}, &created); err != nil {
		return err
	}
	g.commentAuthor = created.User.Login
	return nil
}

func (g *GitHub) ownsComment(ctx context.Context, c comment) (bool, error) {
	if g.p.AppID != 0 {
		return c.PerformedViaApp != nil && c.PerformedViaApp.ID == g.p.AppID, nil
	}
	if g.commentAuthor == "" {
		var user struct{ Login string }
		if err := g.call(ctx, "GET", strings.TrimRight(g.p.APIURL, "/")+"/user", nil, &user); err != nil {
			return false, err
		}
		if user.Login == "" {
			return false, fmt.Errorf("GitHub comment identity unavailable")
		}
		g.commentAuthor = user.Login
	}
	return c.User.Login != "" && c.User.Login == g.commentAuthor, nil
}
