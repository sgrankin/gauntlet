// Package summarize is an optional enricher: given a candidate about to
// land, it asks Claude for a short prose summary of what the branch
// actually did and returns it for use as the merge commit's body
// (internal/queue's buildMergeMessage puts it between the subject and the
// Gauntlet-* trailers).
//
// The one property this package guarantees, unconditionally: MergeBody
// never returns an error and never blocks a landing. Any failure — a git
// error gathering the branch's own history, an HTTP error, a timeout, an
// empty or refused model response — is logged as a single line and
// answered with "". The queue treats that exactly like "no summarizer
// configured": the merge lands with a plain subject + trailers. This
// package is a nicety, not a dependency.
package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
)

// DefaultModel is the model MergeBody calls when Params.Model is empty.
// Haiku-class: per the claude-api skill's guidance, a 2-6 sentence
// summarization task is a single cheap classification-shaped call, not a
// coding or planning workload, so there is no reason to spend Sonnet/Opus
// tier latency or tokens on it. Model is still fully configurable (Params.
// Model / the "summarize { model ... }" config node) for operators who
// want a different tier.
const DefaultModel = "claude-haiku-4-5"

const (
	defaultMaxTokens = 512
	defaultTimeout   = 10 * time.Second
	defaultBaseURL   = "https://api.anthropic.com"
	anthropicVersion = "2023-06-01"
	messagesPath     = "/v1/messages"
)

// systemPrompt instructs the model on exactly the shape of output a merge
// commit body wants: short, plain, no formatting, no restating the obvious.
const systemPrompt = "You summarize a merged branch for a merge commit's body. " +
	"Write 2 to 5 plain sentences describing what the branch did and why, in present tense. " +
	"Do not use bullet lists, headings, or markdown formatting. Do not editorialize or use hype. " +
	"Do not repeat the topic name verbatim as your opening words. " +
	"Respond with only the summary prose and nothing else."

// Git is the minimal read-only git surface Summarizer needs to reconstruct
// a candidate branch's story: the commits it introduces and their
// aggregate diffstat, both over base..tip. internal/gitx.Repo satisfies
// this structurally (Go interfaces are implicit) — Summarizer never
// imports gitx.Repo's constructor or any of its other methods, and tests
// can supply a fake without touching real git.
type Git interface {
	// Log returns, oldest-first, the subject and body of every commit
	// reachable from tip but not base (matching gitx.Repo.Log).
	Log(ctx context.Context, base, tip string) ([]gitx.CommitInfo, error)

	// DiffStat returns the diffstat summary for base..tip (matching
	// gitx.Repo.DiffStat).
	DiffStat(ctx context.Context, base, tip string) (string, error)
}

// Params configures a Summarizer. Only Git and APIKey are meaningfully
// required for MergeBody to ever produce output; every other field has a
// sane default, and a missing/wrong APIKey just degrades to MergeBody
// always returning "" (this package never fails loudly — see the package
// doc).
type Params struct {
	// Git gathers the candidate branch's commits and diffstat.
	Git Git

	// Model is the Claude model ID to call. Empty defaults to
	// DefaultModel.
	Model string

	// APIKey authenticates against the Messages API.
	APIKey string

	// MaxTokens bounds the response length. Zero/negative defaults to
	// defaultMaxTokens — small, since the output is a handful of
	// sentences.
	MaxTokens int

	// Timeout bounds each Messages API call. Zero/negative defaults to
	// defaultTimeout (10s).
	Timeout time.Duration

	// BaseURL overrides the Messages API host. Empty defaults to the
	// real API (defaultBaseURL). Test seam only.
	BaseURL string

	// Log receives one line per degraded MergeBody call (every path that
	// makes it return ""). Nil discards.
	Log io.Writer
}

// Summarizer builds a Claude-written merge-commit body for a candidate
// about to land. The zero value is not usable; construct with New.
type Summarizer struct {
	git       Git
	model     string
	apiKey    string
	maxTokens int
	timeout   time.Duration
	baseURL   string
	log       io.Writer
	client    *http.Client
}

// New constructs a Summarizer, applying defaults for every zero-valued
// optional Params field.
func New(p Params) *Summarizer {
	model := p.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := p.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	baseURL := p.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	log := p.Log
	if log == nil {
		log = io.Discard
	}
	return &Summarizer{
		git:       p.Git,
		model:     model,
		apiKey:    p.APIKey,
		maxTokens: maxTokens,
		timeout:   timeout,
		baseURL:   baseURL,
		log:       log,
		client:    &http.Client{},
	}
}

// MergeBody gathers cand's own commits (base..cand.SHA) and their
// diffstat, asks Claude to summarize what the branch did, and returns the
// trimmed result. On any error, timeout, refusal, or empty response, it
// logs one line to Params.Log and returns "" — see the package doc for why
// that's a hard guarantee, not a best-effort one.
func (s *Summarizer) MergeBody(ctx context.Context, cand core.Candidate, baseOID string) string {
	commits, err := s.git.Log(ctx, baseOID, cand.SHA)
	if err != nil {
		s.logf("summarize: log %s..%s: %v", baseOID, cand.SHA, err)
		return ""
	}
	if len(commits) == 0 {
		s.logf("summarize: no commits in %s..%s for %s, skipping", baseOID, cand.SHA, cand.Ref)
		return ""
	}
	diffstat, err := s.git.DiffStat(ctx, baseOID, cand.SHA)
	if err != nil {
		s.logf("summarize: diffstat %s..%s: %v", baseOID, cand.SHA, err)
		return ""
	}

	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	text, err := s.call(cctx, buildPrompt(cand, commits, diffstat))
	if err != nil {
		s.logf("summarize: %s: %v", cand.Ref, err)
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "" {
		s.logf("summarize: empty response for %s", cand.Ref)
		return ""
	}
	return text
}

func (s *Summarizer) logf(format string, args ...any) {
	fmt.Fprintf(s.log, format+"\n", args...)
}

// buildPrompt renders the user-turn content: topic/user, then each
// commit's subject (and indented body, if any), then the diffstat.
func buildPrompt(cand core.Candidate, commits []gitx.CommitInfo, diffstat string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Topic: %s\n", cand.Topic)
	if cand.User != "" {
		fmt.Fprintf(&b, "Author: %s\n", cand.User)
	}
	b.WriteString("\nCommits:\n")
	for _, c := range commits {
		fmt.Fprintf(&b, "- %s\n", c.Subject)
		if c.Body != "" {
			for _, line := range strings.Split(c.Body, "\n") {
				fmt.Fprintf(&b, "  %s\n", line)
			}
		}
	}
	b.WriteString("\nDiffstat:\n")
	b.WriteString(diffstat)
	b.WriteString("\n")
	return b.String()
}

// messageRequest and messageResponse are the minimal Messages API request
// and response shapes this package needs — see the claude-api skill for
// the full request/response reference. No thinking/effort fields: Haiku
// (and Haiku-tier models generally) doesn't support them, and this task
// doesn't need them regardless.
type messageRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system,omitempty"`
	Messages  []requestMessage `json:"messages"`
}

type requestMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messageResponse struct {
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// call issues one Messages API request and returns the concatenated text
// of every text content block. Any non-2xx status, refusal, or transport
// error is returned as an error (MergeBody is what turns that into "").
func (s *Summarizer) call(ctx context.Context, userPrompt string) (string, error) {
	reqBody := messageRequest{
		Model:     s.model,
		MaxTokens: s.maxTokens,
		System:    systemPrompt,
		Messages:  []requestMessage{{Role: "user", Content: userPrompt}},
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+messagesPath, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", s.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("api status %d: %s", resp.StatusCode, strings.TrimSpace(string(respData)))
	}

	var parsed messageResponse
	if err := json.Unmarshal(respData, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if parsed.StopReason == "refusal" {
		return "", fmt.Errorf("model refused the request")
	}
	var out strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String(), nil
}
