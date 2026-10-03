package policy

import (
	"context"
	"testing"
	"time"
)

const testPolicy = `package gauntlet
import rego.v1
submission := {"allow": allowed, "requirements": [{"name":"owner", "satisfied":allowed,"reason":"write permission required"}]} if {
 allowed := input.permission == "write"
}`

func TestNamedDecisionAndMissingFactsFailClosed(t *testing.T) {
	engine, err := Compile(context.Background(), testPolicy, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		input   any
		allow   bool
		wantErr bool
	}{{map[string]any{"permission": "write"}, true, false}, {map[string]any{"permission": "read"}, false, false}, {map[string]any{}, false, true}} {
		decision, err := engine.Decide(context.Background(), "submission", tc.input)
		if (err != nil) != tc.wantErr || decision.Allow != tc.allow {
			t.Fatalf("input=%v decision=%+v err=%v", tc.input, decision, err)
		}
		if err == nil && !decision.Allow && decision.Reason() == "" {
			t.Fatal("denial lacks reason")
		}
	}
}
func TestNetworkCapabilitiesUnavailable(t *testing.T) {
	for _, source := range []string{`package gauntlet
submission := http.send({"method":"GET","url":"https://example.com"})`, `package gauntlet
submission := opa.runtime()`, `package gauntlet
submission := time.now_ns()`} {
		if _, err := Compile(context.Background(), source, time.Second); err == nil {
			t.Fatalf("external capability compiled: %s", source)
		}
	}
}
func TestMalformedPolicyDecisionDenied(t *testing.T) {
	for _, source := range []string{`package gauntlet
submission := true`, `package gauntlet
submission := {"allow":true}`, `package gauntlet
submission := {"allow":true,"requirements":[{"name":"required","satisfied":false,"reason":"missing"}]}`} {
		engine, err := Compile(context.Background(), source, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := engine.Decide(context.Background(), "submission", map[string]any{})
		if err == nil && decision.Allow {
			t.Fatalf("malformed/unsatisfied decision allowed: %s", source)
		}
	}
}

func TestDefaultCommandAuthorityAndExplicitReplacement(t *testing.T) {
	ctx := context.Background()
	input := Input{SchemaVersion: 1, Principal: Principal{Source: "github", ID: "reader", Authenticated: true, Permission: "read"}, Command: Command{Kind: "merge"}}
	decision, err := Default().Decide(ctx, "command", input)
	if err != nil || decision.Allow {
		t.Fatalf("reader allowed: %+v %v", decision, err)
	}
	input.Command.Kind = "check"
	decision, err = Default().Decide(ctx, "command", input)
	if err != nil || !decision.Allow {
		t.Fatalf("reader cannot diagnose: %+v %v", decision, err)
	}
	engine, err := Compile(ctx, `package gauntlet
command := {"allow":true,"requirements":[]}`, time.Second, Options{Replace: []string{"command"}})
	if err != nil {
		t.Fatal(err)
	}
	input.Command.Kind = "merge"
	decision, err = engine.Decide(ctx, "command", input)
	if err != nil || !decision.Allow {
		t.Fatalf("replacement still has hidden gate: %+v %v", decision, err)
	}
}

func TestExtensionKeepsDefaultAndMissingReplacementDenies(t *testing.T) {
	ctx := context.Background()
	input := Input{SchemaVersion: 1, Principal: Principal{Source: "github", Authenticated: true, Permission: "read"}}
	for _, tc := range []struct {
		source  string
		opts    Options
		wantErr bool
	}{
		{`package gauntlet
command := {"allow":true,"requirements":[]}`, Options{Extend: []string{"command"}}, false},
		{`package gauntlet
unrelated := true`, Options{Replace: []string{"command"}}, true},
	} {
		engine, err := Compile(ctx, tc.source, time.Second, tc.opts)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := engine.Decide(ctx, "command", input)
		if decision.Allow || (err != nil) != tc.wantErr {
			t.Fatalf("decision=%+v err=%v", decision, err)
		}
	}
}

func TestDefaultRetryThreshold(t *testing.T) {
	for _, tc := range []struct {
		action     string
		confidence float64
		allow      bool
	}{{"retry", .9, true}, {"retry", .5, false}, {"abort", 1, false}, {"unknown", 1, false}} {
		d, err := Default().Decide(context.Background(), "retry", Input{Retry: map[string]any{"action": tc.action, "confidence": tc.confidence, "min_confidence": .8}})
		if err != nil || d.Allow != tc.allow {
			t.Fatalf("%+v => %+v %v", tc, d, err)
		}
	}
}

func TestSharedGitHubFactsMissingDeny(t *testing.T) {
	decision, err := Default().Decide(context.Background(), "submission", Input{Candidate: map[string]any{"source": "github"}})
	if err != nil || decision.Allow {
		t.Fatalf("missing forge facts allowed: %+v %v", decision, err)
	}
}

func TestCustomModuleCannotAlterUnselectedDefaults(t *testing.T) {
	engine, err := Compile(context.Background(), `package gauntlet.defaults
import rego.v1
authorized if true`, time.Second, Options{Replace: []string{"submission"}})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := engine.Decide(context.Background(), "command", Input{Principal: Principal{Source: "github", Authenticated: true, Permission: "read"}, Command: Command{Kind: "merge"}})
	if err != nil || decision.Allow {
		t.Fatalf("custom module changed an unselected default: %+v %v", decision, err)
	}
}
