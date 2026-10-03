// Package policy evaluates operator-owned Rego without external capabilities.
package policy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
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

//go:embed defaults.rego
var ruleFiles embed.FS

type Options struct{ Replace, Extend []string }

var Names = []string{"command", "submission", "execution", "deployment", "retry"}

type Engine struct {
	queries    map[string]rego.PreparedEvalQuery
	extensions map[string]rego.PreparedEvalQuery
	custom     map[string]bool
	Version    string
	Timeout    time.Duration
}

var defaultOnce sync.Once
var defaultEngine *Engine

func Default() *Engine {
	defaultOnce.Do(func() {
		var err error
		defaultEngine, err = Compile(context.Background(), "", 100*time.Millisecond)
		if err != nil {
			panic(err)
		}
	})
	return defaultEngine
}

func Compile(ctx context.Context, source string, timeout time.Duration, options ...Options) (*Engine, error) {
	if len(source) > 1<<20 {
		return nil, fmt.Errorf("policy exceeds 1048576 bytes")
	}
	if timeout <= 0 {
		timeout = 100 * time.Millisecond
	}
	opts := Options{}
	if source != "" {
		opts.Extend = []string{"submission"}
	}
	if len(options) > 0 {
		opts = options[0]
	}
	if len(opts.Replace)+len(opts.Extend) > 0 && source == "" {
		return nil, fmt.Errorf("custom decisions require Rego source")
	}
	seen := map[string]bool{}
	for _, name := range append(slices.Clone(opts.Replace), opts.Extend...) {
		if !slices.Contains(Names, name) || seen[name] {
			return nil, fmt.Errorf("unknown or duplicate policy decision %q", name)
		}
		seen[name] = true
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
	defaults, _ := ruleFiles.ReadFile("defaults.rego")
	e := &Engine{queries: map[string]rego.PreparedEvalQuery{}, extensions: map[string]rego.PreparedEvalQuery{}, custom: seen, Timeout: timeout}
	prepare := func(query string, operator bool) (rego.PreparedEvalQuery, error) {
		args := []func(*rego.Rego){rego.Query(query), rego.Module("defaults.rego", string(defaults)), rego.Capabilities(caps), rego.StrictBuiltinErrors(true)}
		if operator {
			args = append(args, rego.Module("operator.rego", source))
		}
		return rego.New(args...).PrepareForEval(ctx)
	}
	for _, name := range Names {
		query := "data.gauntlet.defaults." + name
		if slices.Contains(opts.Replace, name) {
			query = "data.gauntlet." + name
		}
		prepared, err := prepare(query, slices.Contains(opts.Replace, name))
		if err != nil {
			return nil, fmt.Errorf("compile %s policy: %w", name, err)
		}
		e.queries[name] = prepared
		if slices.Contains(opts.Extend, name) {
			prepared, err = prepare("data.gauntlet."+name, true)
			if err != nil {
				return nil, fmt.Errorf("compile %s extension: %w", name, err)
			}
			e.extensions[name] = prepared
		}
	}
	data, _ := json.Marshal(struct {
		Defaults, Source string
		Options          Options
	}{string(defaults), source, opts})
	sum := sha256.Sum256(data)
	e.Version = hex.EncodeToString(sum[:])
	return e, nil
}

func (e *Engine) HasCustom(name string) bool { return e.custom[name] }
func (e *Engine) Decide(ctx context.Context, name string, input any) (Decision, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return Decision{}, fmt.Errorf("invalid policy facts: %w", err)
	}
	if len(data) > 1<<20 {
		return Decision{}, fmt.Errorf("policy facts exceed 1048576 bytes")
	}
	var normalized any
	if err := json.Unmarshal(data, &normalized); err != nil {
		return Decision{}, err
	}
	input = normalized
	query, ok := e.queries[name]
	if !ok {
		return Decision{}, fmt.Errorf("unknown policy decision %q", name)
	}
	ctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	decision, err := e.evaluate(ctx, query, input)
	if err != nil {
		return Decision{}, err
	}
	if extension, ok := e.extensions[name]; ok {
		extra, err := e.evaluate(ctx, extension, input)
		if err != nil {
			return Decision{}, err
		}
		decision.Allow = decision.Allow && extra.Allow
		decision.Requirements = append(decision.Requirements, extra.Requirements...)
	}
	return decision, nil
}
func (e *Engine) evaluate(ctx context.Context, query rego.PreparedEvalQuery, input any) (Decision, error) {
	results, err := query.Eval(ctx, rego.EvalInput(input))
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
		return "policy denied"
	}
	return strings.Join(reasons, "; ")
}
