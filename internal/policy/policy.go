// Package policy evaluates operator-owned Rego without external capabilities.
package policy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
	"io"
	"strings"
	"time"
)

type Requirement struct {
	Name      string `json:"name"`
	Satisfied bool   `json:"satisfied"`
	Reason    string `json:"reason"`
}
type Decision struct {
	Allow        bool          `json:"allow"`
	Requirements []Requirement `json:"requirements"`
	Version      string        `json:"-"`
}
type Engine struct {
	query   rego.PreparedEvalQuery
	Version string
	Timeout time.Duration
}

func Compile(ctx context.Context, source string, timeout time.Duration) (*Engine, error) {
	if len(source) == 0 || len(source) > 1<<20 {
		return nil, fmt.Errorf("policy must contain 1..1048576 bytes")
	}
	caps := ast.CapabilitiesForThisVersion()
	allowed := caps.Builtins[:0]
	for _, builtin := range caps.Builtins {
		if !builtin.Nondeterministic && builtin.Name != "http.send" && builtin.Name != "opa.runtime" && !strings.HasPrefix(builtin.Name, "net.") {
			allowed = append(allowed, builtin)
		}
	}
	caps.Builtins = allowed
	caps.AllowNet = []string{}
	query, err := rego.New(rego.Query("data.gauntlet.decision"), rego.Module("operator.rego", source), rego.Capabilities(caps), rego.StrictBuiltinErrors(true)).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("compile admission policy: %w", err)
	}
	sum := sha256.Sum256([]byte(source))
	return &Engine{query: query, Version: hex.EncodeToString(sum[:]), Timeout: timeout}, nil
}
func (e *Engine) Evaluate(ctx context.Context, input any) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	results, err := e.query.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return Decision{}, fmt.Errorf("policy evaluation: %w", err)
	}
	if len(results) != 1 || len(results[0].Expressions) != 1 {
		return Decision{}, fmt.Errorf("policy decision is missing or ambiguous")
	}
	data, err := json.Marshal(results[0].Expressions[0].Value)
	if err != nil {
		return Decision{}, err
	}
	if len(data) > 65536 {
		return Decision{}, fmt.Errorf("policy decision too large")
	}
	var wire struct {
		Allow        *bool         `json:"allow"`
		Requirements []Requirement `json:"requirements"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF || wire.Allow == nil || wire.Requirements == nil || len(wire.Requirements) > 128 {
		return Decision{}, fmt.Errorf("policy must return allow and named requirements")
	}
	decision := Decision{Allow: *wire.Allow, Requirements: wire.Requirements, Version: e.Version}
	for _, requirement := range decision.Requirements {
		if requirement.Name == "" || len(requirement.Name) > 256 || len(requirement.Reason) > 2048 {
			return Decision{}, fmt.Errorf("invalid policy requirement")
		}
		if !requirement.Satisfied {
			decision.Allow = false
		}
	}
	return decision, nil
}
func (d Decision) Reason() string {
	var reasons []string
	for _, r := range d.Requirements {
		if !r.Satisfied {
			reasons = append(reasons, r.Name+": "+r.Reason)
		}
	}
	if len(reasons) == 0 && !d.Allow {
		return "policy denied admission"
	}
	return strings.Join(reasons, "; ")
}
