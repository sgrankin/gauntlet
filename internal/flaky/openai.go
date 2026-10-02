package flaky

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
)

// OpenAI uses the Responses API without tools or server-side response storage.
type OpenAI struct {
	Model, Token, BaseURL string
	MaxOutputBytes        int
	Client                *http.Client
}

func (o OpenAI) Classify(ctx context.Context, job core.CheckJob, res core.CheckResult) (Decision, error) {
	body, _ := json.Marshal(map[string]any{
		"model":             o.Model,
		"instructions":      instructions,
		"input":             evidence(job, res, o.MaxOutputBytes),
		"store":             false,
		"max_output_tokens": 2048,
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "failure_action", "strict": true, "schema": schema,
		}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/responses", bytes.NewReader(body))
	if err != nil {
		return Decision{}, fmt.Errorf("build classification request")
	}
	req.Header.Set("Authorization", "Bearer "+o.Token)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{}
	if o.Client != nil {
		client = *o.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return Decision{}, fmt.Errorf("classification request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Decision{}, fmt.Errorf("classification HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return Decision{}, fmt.Errorf("invalid classification response")
	}
	var wire struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(data, &wire) != nil || wire.Status != "completed" {
		return Decision{}, fmt.Errorf("classification response not completed")
	}
	var texts []string
	for _, out := range wire.Output {
		if out.Type != "message" {
			continue
		}
		for _, content := range out.Content {
			if content.Type == "refusal" {
				return Decision{}, fmt.Errorf("classification refused")
			}
			if content.Type == "output_text" {
				texts = append(texts, content.Text)
			}
		}
	}
	if len(texts) != 1 {
		return Decision{}, fmt.Errorf("classification did not return one decision")
	}
	return decodeDecision([]byte(texts[0]))
}
