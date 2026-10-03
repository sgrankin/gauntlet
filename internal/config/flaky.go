package config

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// FailureReview is operator-owned: only named, repeatable checks may send
// failure output to a model and retry inside their existing queue run.
type FailureReview struct {
	Tools          bool          `kdl:"tools"`
	MaxRunRetries  int           `kdl:"max-run-retries"`
	Auth           string        `kdl:"auth"`
	Model          string        `kdl:"model"`
	TokenEnv       string        `kdl:"token-env"`
	APIURL         string        `kdl:"api-url"`
	Codex          string        `kdl:"codex"`
	Checks         []string      `kdl:"checks"`
	MaxRetries     int           `kdl:"max-retries"`
	MinConfidence  float64       `kdl:"min-confidence"`
	Timeout        time.Duration `kdl:"timeout,format:units"`
	MaxOutputBytes int           `kdl:"max-output-bytes"`
}

func (f *FailureReview) defaults() {
	if f.Auth == "" {
		f.Auth = "api-key"
	}
	if f.TokenEnv == "" {
		if f.Auth == "chatgpt" {
			f.TokenEnv = "CODEX_ACCESS_TOKEN"
		} else {
			f.TokenEnv = "OPENAI_API_KEY"
		}
	}
	if f.APIURL == "" {
		f.APIURL = "https://api.openai.com/v1"
	}
	if f.Codex == "" {
		f.Codex = "codex"
	}
	if f.MaxRunRetries == 0 {
		f.MaxRunRetries = 3
	}
	if f.MaxRetries == 0 {
		f.MaxRetries = 1
	}
	if f.MinConfidence == 0 {
		f.MinConfidence = .8
	}
	if f.Timeout == 0 {
		f.Timeout = 30 * time.Second
	}
	if f.MaxOutputBytes == 0 {
		f.MaxOutputBytes = 16384
	}
}

func (f *FailureReview) validate() error {
	if f.Auth != "api-key" && f.Auth != "chatgpt" {
		return fmt.Errorf("failure-review: auth must be api-key or chatgpt")
	}
	if strings.TrimSpace(f.Model) == "" {
		return fmt.Errorf("failure-review: model is required")
	}
	if strings.TrimSpace(f.TokenEnv) == "" || strings.ContainsAny(f.TokenEnv, "=\x00") {
		return fmt.Errorf("failure-review: token-env must name an environment variable")
	}
	if len(f.Checks) == 0 {
		return fmt.Errorf("failure-review: checks must explicitly name repeatable checks")
	}
	seen := map[string]bool{}
	for _, name := range f.Checks {
		if name == "" || strings.HasPrefix(name, "image:") || strings.HasPrefix(name, "receipt:") || seen[name] {
			return fmt.Errorf("failure-review: invalid or duplicate check %q", name)
		}
		seen[name] = true
	}
	if f.MaxRunRetries < 1 || f.MaxRunRetries > 20 {
		return fmt.Errorf("failure-review: max-run-retries must be 1..20")
	}
	if f.MaxRetries < 1 || f.MaxRetries > 3 {
		return fmt.Errorf("failure-review: max-retries must be 1..3")
	}
	if math.IsNaN(f.MinConfidence) || math.IsInf(f.MinConfidence, 0) || f.MinConfidence <= 0 || f.MinConfidence > 1 {
		return fmt.Errorf("failure-review: min-confidence must be >0 and <=1")
	}
	if f.Timeout <= 0 || f.Timeout > 5*time.Minute {
		return fmt.Errorf("failure-review: timeout must be >0 and <=5m")
	}
	if f.MaxOutputBytes < 256 || f.MaxOutputBytes > 65536 {
		return fmt.Errorf("failure-review: max-output-bytes must be 256..65536")
	}
	u, err := url.Parse(f.APIURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("failure-review: api-url must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if strings.TrimSpace(f.Codex) == "" {
		return fmt.Errorf("failure-review: codex must name an executable")
	}
	return nil
}
