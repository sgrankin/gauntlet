package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

type GerritParams struct {
	APIURL, Project, Username, Token, VerificationRequirement string
	Targets                                                   map[string]string
	Git                                                       interface {
		Git
		ReviewBaseLanded(context.Context, string, string) (bool, error)
	}
}

type Gerrit struct {
	p      GerritParams
	client *http.Client
}

func NewGerrit(p GerritParams) *Gerrit {
	if p.VerificationRequirement == "" {
		p.VerificationRequirement = "Verified"
	}
	return &Gerrit{p: p, client: &http.Client{Timeout: 10 * time.Second}}
}

type requirement struct{ Name, Status string }
type change struct {
	ID                      string
	Number                  int `json:"_number"`
	Branch, Status, Subject string
	WorkInProgress          bool   `json:"work_in_progress"`
	CurrentRevision         string `json:"current_revision"`
	More                    bool   `json:"_more_changes"`
	Revisions               map[string]struct {
		Ref    string
		Commit struct {
			Message string
			Parents []struct{ Commit string }
		}
	}
	Requirements []requirement `json:"submit_requirements"`
}

func (g *Gerrit) call(ctx context.Context, method, path string, input, output any) error {
	var data []byte
	if input != nil {
		var err error
		data, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.p.APIURL, "/")+"/a"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	r.SetBasicAuth(g.p.Username, g.p.Token)
	r.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("gerrit %s: HTTP %d", method, resp.StatusCode)
	}
	if output == nil {
		return nil
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	// Gerrit prefixes JSON with its anti-XSSI guard.
	if bytes.HasPrefix(data, []byte(")]}'")) {
		_, data, _ = bytes.Cut(data, []byte("\n"))
	}
	return json.Unmarshal(data, output)
}

const changeOptions = "o=CURRENT_REVISION&o=CURRENT_COMMIT&o=SUBMIT_REQUIREMENTS"

var gerritChangeID = regexp.MustCompile(`(?m)^Change-Id: I[0-9a-fA-F]{40}\r?$`)

func (g *Gerrit) eligible(c change, includeVerification bool) bool {
	if c.Status != "NEW" || c.WorkInProgress || len(c.Requirements) == 0 {
		return false
	}
	for _, r := range c.Requirements {
		if !includeVerification && r.Name == g.p.VerificationRequirement {
			continue
		}
		if r.Status != "SATISFIED" && r.Status != "OVERRIDDEN" && r.Status != "NOT_APPLICABLE" {
			return false
		}
	}
	return true
}

func gerritSlot(target string, number int) string {
	return fmt.Sprintf("refs/heads/for/%s/gerrit/change-%010d", target, number)
}

func (g *Gerrit) Candidates(ctx context.Context) ([]core.Candidate, error) {
	var all []change
	for start := 0; ; {
		var rows []change
		query := "project:" + g.p.Project + " status:open"
		if err := g.call(ctx, "GET", "/changes/?q="+url.QueryEscape(query)+"&n=100&S="+strconv.Itoa(start)+"&"+changeOptions, nil, &rows); err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) == 0 || !rows[len(rows)-1].More {
			break
		}
		start += len(rows)
		if start > 100000 {
			return nil, fmt.Errorf("gerrit pagination exceeds limit")
		}
	}
	bySHA := map[string]change{}
	for _, c := range all {
		bySHA[c.CurrentRevision] = c
	}
	var out []core.Candidate
	for _, c := range all {
		target, ok := g.p.Targets[c.Branch]
		if !ok || !g.eligible(c, false) {
			continue
		}
		r := c.Revisions[c.CurrentRevision]
		message := strings.TrimSpace(r.Commit.Message)
		footer := message
		if i := strings.LastIndex(message, "\n\n"); i >= 0 {
			footer = message[i+2:]
		}
		if len(gerritChangeID.FindAllString(footer, -1)) != 1 {
			return nil, fmt.Errorf("Gerrit change %d must have exactly one Change-Id trailer", c.Number)
		}
		if len(r.Commit.Parents) != 1 {
			return nil, fmt.Errorf("Gerrit change %d must have one parent", c.Number)
		}
		base := r.Commit.Parents[0].Commit
		candidate := core.Candidate{Ref: gerritSlot(target, c.Number), Target: target, Topic: c.Subject, SHA: c.CurrentRevision, Source: "gerrit", SourceBase: base, Message: r.Commit.Message,
			ReviewURL: strings.TrimRight(g.p.APIURL, "/") + "/c/" + g.p.Project + "/+/" + strconv.Itoa(c.Number)}
		if parent, ok := bySHA[base]; ok {
			if parent.Branch != c.Branch {
				continue
			}
			candidate.DependsOn = gerritSlot(target, parent.Number)
		} else {
			if err := g.p.Git.FetchReview(ctx, r.Ref, fmt.Sprintf("refs/gauntlet/reviews/gerrit/%d", c.Number), c.CurrentRevision); err != nil {
				return nil, err
			}
			landed, err := g.p.Git.ReviewBaseLanded(ctx, c.Branch, base)
			if err != nil {
				return nil, err
			}
			if !landed {
				continue
			}
		}
		candidate.Version = version(candidate, 0)
		if err := g.p.Git.FetchReview(ctx, r.Ref, fmt.Sprintf("refs/gauntlet/reviews/gerrit/%d", c.Number), c.CurrentRevision); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func (g *Gerrit) Validate(ctx context.Context, c core.Candidate) error {
	_, tail, _ := strings.Cut(c.Ref, "/gerrit/change-")
	n, err := strconv.Atoi(tail)
	if err != nil {
		return fmt.Errorf("invalid Gerrit review slot")
	}
	path := "/changes/" + strconv.Itoa(n)
	var current change
	if err := g.call(ctx, "GET", path+"/detail?"+changeOptions, nil, &current); err != nil {
		return err
	}
	if current.CurrentRevision != c.SHA || current.Revisions[c.SHA].Commit.Message != c.Message || !g.eligible(current, false) {
		return fmt.Errorf("Gerrit patch set or submit requirements changed")
	}
	// Vote on the exact original patch set, never whichever revision happens
	// to be current when the request arrives. Only the queue's all-green
	// landing path reaches this operation.
	if err := g.call(ctx, "POST", path+"/revisions/"+c.SHA+"/review", map[string]any{"labels": map[string]int{"Verified": 1}}, nil); err != nil {
		return err
	}
	if err := g.call(ctx, "GET", path+"/detail?"+changeOptions, nil, &current); err != nil {
		return err
	}
	if current.CurrentRevision != c.SHA || !g.eligible(current, true) {
		return fmt.Errorf("Gerrit change is not submittable after verification")
	}
	return nil
}

// A successful target push closes matching changes natively through Gerrit's
// Change-Id processing. Recovery checks that the server recorded that fact.
func (g *Gerrit) Landed(ctx context.Context, c core.Candidate, commit string) error {
	_, tail, _ := strings.Cut(c.Ref, "/gerrit/change-")
	n, err := strconv.Atoi(tail)
	if err != nil {
		return err
	}
	var current change
	if err := g.call(ctx, "GET", "/changes/"+strconv.Itoa(n)+"/detail?"+changeOptions, nil, &current); err != nil {
		return err
	}
	if current.Status != "MERGED" {
		return fmt.Errorf("target advanced but Gerrit has not recorded the change as merged")
	}
	return nil
}
