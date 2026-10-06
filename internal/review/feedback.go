package review

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// Feedback is advisory. Failure to publish a comment never changes a verdict.
func (g *GitHub) Feedback(ctx context.Context, ev core.Event) error {
	if g.feedbackQueue != nil {
		if (ev.Record == nil && ev.Kind != core.EventTrialClean) || ev.Candidate.Source != "github" {
			return nil
		}
		if ev.Record != nil {
			copy := *ev.Record
			copy.Checks = append([]core.CheckResult(nil), copy.Checks...)
			ev.Record = &copy
		}
		select {
		case g.feedbackQueue <- ev:
			return nil
		default:
			return fmt.Errorf("review feedback queue full")
		}
	}
	return g.feedbackSync(ctx, ev)
}

// StartFeedback keeps advisory forge writes off the reconcile goroutine.
// The returned completion channel is joined during daemon shutdown.
func (g *GitHub) StartFeedback(ctx context.Context) <-chan struct{} {
	g.feedbackQueue = make(chan core.Event, 128)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-g.feedbackQueue:
				fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				err := g.feedbackSync(fctx, ev)
				cancel()
				if err != nil {
					log.Printf("github failure feedback: %v", err)
				}
			}
		}
	}()
	return done
}

func (g *GitHub) feedbackSync(ctx context.Context, ev core.Event) error {
	if ev.Candidate.Source != "github" {
		return nil
	}
	if ev.Record == nil {
		if ev.Kind == core.EventTrialClean {
			return g.feedback(ctx, ev.Candidate, "Gauntlet is verifying this revision on the proposed target history."+g.progressLink(ev.Candidate))
		}
		return nil
	}
	rec := ev.Record
	if rec.Outcome == core.OutcomeLanded {
		url := strings.Split(ev.Candidate.ReviewURL, "/pull/")[0] + "/commit/" + rec.MergeSHA
		body := fmt.Sprintf("Landed by Gauntlet as [%s](%s).\n\n%s", rec.MergeSHA, url, safeMarkdown(rec.Detail))
		body += "\n\n<!-- gauntlet:landed:" + rec.MergeSHA + " -->" + g.progressLink(ev.Candidate)
		return g.feedback(ctx, ev.Candidate, body, true)
	}
	body := fmt.Sprintf("Gauntlet could not land this revision (`%s`).\n\n%s", ev.Candidate.SHA, safeMarkdown(rec.Detail))
	if rec.BatchID != "" {
		body += "\n\nThis was a batch failure. Attribution to this PR is **unconfirmed**; changes will be checked separately."
	}
	runID := rec.RunID
	if rec.BatchID != "" {
		runID = rec.BatchID
	}
	runURL := ""
	if g.p.DashboardURL != "" && runID != "" {
		runURL = strings.TrimRight(g.p.DashboardURL, "/") + "/run/" + url.PathEscape(runID)
		body += "\n\n[Run and failure details](" + runURL + ")."
	}
	for _, check := range rec.Checks {
		if check.Err == nil && check.Status != core.CheckFailed {
			continue
		}
		output := check.Output
		if len(output) > 6000 {
			output = output[len(output)-6000:]
		}
		body += "\n\n**" + safeMarkdown(check.Name) + "**\n\n<pre>" + safeMarkdown(output) + "</pre>"
		if runURL != "" && check.LogPath != "" {
			body += "\n\n[Full log](" + runURL + "/log/" + url.PathEscape(check.Name) + ")."
		}
		if check.Err != nil {
			body += "\n\nInfrastructure error: " + safeMarkdown(check.Err.Error())
		}
	}
	if rec.MergeSHA != "" {
		body += "\n\nConstructed revision: `" + rec.MergeSHA + "`."
	}
	return g.feedback(ctx, ev.Candidate, body+g.progressLink(ev.Candidate))
}

func safeMarkdown(s string) string {
	// Render untrusted output as inert text: no HTML, mentions, or bot commands.
	s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "@", "&#64;", "`", "&#96;", "*", "&#42;", "[", "&#91;").Replace(s)
	if len(s) > 20000 {
		s = s[:20000]
	}
	return s
}

func (g *GitHub) feedback(ctx context.Context, c core.Candidate, body string, allowClosed ...bool) error {
	return g.commentFeedback(ctx, c, body, "<!-- gauntlet:feedback -->", len(allowClosed) != 0 && allowClosed[0])
}

func (g *GitHub) commentFeedback(ctx context.Context, c core.Candidate, body, marker string, allowClosed bool) error {
	g.feedbackMu.Lock()
	defer g.feedbackMu.Unlock()
	n, err := pullNumber(c.Ref)
	if err != nil {
		return err
	}
	p, err := g.getPull(ctx, n)
	if err != nil {
		return err
	}
	if (p.State != "open" && !allowClosed) || p.Head.SHA != c.SHA {
		return nil
	}
	body += "\n\n" + marker
	comments, err := pages[comment](ctx, g, fmt.Sprintf("/issues/%d/comments", n))
	if err != nil {
		return err
	}
	for i := len(comments) - 1; i >= 0; i-- {
		v := comments[i]
		if !strings.Contains(v.Body, marker) {
			continue
		}
		owned, err := g.ownsComment(ctx, v)
		if err != nil {
			return err
		}
		if !owned {
			continue
		}
		if marker == "<!-- gauntlet:feedback -->" {
			var current bool
			body, current = preserveReceipt(c, v.Body, body)
			if !current {
				return nil
			}
		}
		if v.Body == body {
			return nil
		}
		err = g.call(ctx, "PATCH", g.endpoint(fmt.Sprintf("/issues/comments/%d", v.ID)), map[string]string{"body": body}, nil)
		if err == nil {
			return nil
		}
		if e, ok := err.(*apiError); !ok || (e.status != 403 && e.status != 404) {
			return err
		}
	}
	err = g.call(ctx, "POST", g.endpoint(fmt.Sprintf("/issues/%d/comments", n)), map[string]string{"body": body}, nil)
	return err
}

func (g *GitHub) admissionFeedback(ctx context.Context, p pull, target, reason string) {
	c := core.Candidate{Ref: slot(target, p.Number), SHA: p.Head.SHA}
	c.Target = target
	if err := g.feedback(ctx, c, "Gauntlet admission blocked: "+safeMarkdown(reason)+".\n\nThe request remains pending; gates are rechecked automatically."+g.progressLink(c)); err != nil {
		log.Printf("github admission feedback: %v", err)
	}
}
