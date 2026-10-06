package review

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
)

var receiptHeader = regexp.MustCompile(`^<!-- gauntlet:request:(\d+):([a-zA-Z0-9]+):([a-f0-9]{64}) -->\n[^\n]*\n\n`)

func requestIdentity(r request) string {
	data, _ := json.Marshal([]any{r.Action, r.Count, r.Urgent, r.SkipChecks, r.OverridePause, r.Requester, r.Reason})
	return fmt.Sprintf("%d:%x", r.ID, sha256.Sum256(data))
}

func (g *GitHub) progressLink(c core.Candidate) string {
	if g.p.DashboardURL == "" || c.Target == "" || c.Ref == "" {
		return ""
	}
	query := url.Values{"target": {c.Target}, "ref": {c.Ref}}
	link := strings.TrimRight(g.p.DashboardURL, "/") + "/change?" + query.Encode()
	return "\n\n[Follow progress](" + link + ")."
}

func (g *GitHub) acknowledge(ctx context.Context, r request, target string, comments []comment) error {
	_, digest, _ := strings.Cut(requestIdentity(r), ":")
	marker := fmt.Sprintf("<!-- gauntlet:request:%d:%s:%s -->", r.ID, r.PR.Head.SHA, digest)
	g.feedbackMu.Lock()
	for _, c := range comments {
		if !strings.HasPrefix(c.Body, marker+"\n") {
			continue
		}
		owned, err := g.ownsComment(ctx, c)
		if err != nil || owned {
			g.feedbackMu.Unlock()
			return err
		}
	}
	g.feedbackMu.Unlock()
	c := core.Candidate{Ref: slot(target, r.PR.Number), Target: target, SHA: r.PR.Head.SHA}
	commandURL := fmt.Sprintf("%s#issuecomment-%d", r.PR.HTMLURL, r.ID)
	body := fmt.Sprintf("%s\n[Merge request](%s) for revision `%s`.\n\nRequest received; waiting for admission and verification. The request will be rechecked against current policy before landing.", marker, commandURL, c.SHA)
	if r.Action == "cancel" {
		body = fmt.Sprintf("%s\n[Queue request](%s) for revision `%s`.\n\nThis PR's merge request was withdrawn.", marker, commandURL, c.SHA)
	}
	return g.feedback(ctx, c, body+g.progressLink(c))
}

// A delayed result for the same SHA must not replace a newer request's receipt.
// Later stack commands may take over an earlier member's request.
func preserveReceipt(c core.Candidate, existing, body string) (string, bool) {
	if receiptHeader.MatchString(body) {
		return body, true
	}
	match := receiptHeader.FindStringSubmatch(existing)
	if match == nil || match[2] != c.SHA {
		return body, true
	}
	if c.RequestID != "" {
		currentID, _ := strconv.ParseInt(match[1], 10, 64)
		id, digest, _ := strings.Cut(c.RequestID, ":")
		resultID, err := strconv.ParseInt(id, 10, 64)
		if err != nil || currentID > resultID || (currentID == resultID && digest != match[3]) {
			return "", false
		}
	}
	return match[0] + body, true
}
