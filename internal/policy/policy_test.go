package policy

import (
	"context"
	"testing"
	"time"
)

const testPolicy = `package gauntlet
import rego.v1
decision := {"allow": allowed, "requirements": [{"name":"owner", "satisfied":allowed,"reason":"write permission required"}]} if {
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
		decision, err := engine.Evaluate(context.Background(), tc.input)
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
decision := http.send({"method":"GET","url":"https://example.com"})`, `package gauntlet
decision := opa.runtime()`, `package gauntlet
decision := time.now_ns()`} {
		if _, err := Compile(context.Background(), source, time.Second); err == nil {
			t.Fatalf("external capability compiled: %s", source)
		}
	}
}
func TestMalformedPolicyDecisionDenied(t *testing.T) {
	for _, source := range []string{`package gauntlet
decision := true`, `package gauntlet
decision := {"allow":true}`, `package gauntlet
decision := {"allow":true,"requirements":[{"name":"required","satisfied":false,"reason":"missing"}]}`} {
		engine, err := Compile(context.Background(), source, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := engine.Evaluate(context.Background(), map[string]any{})
		if err == nil && decision.Allow {
			t.Fatalf("malformed/unsatisfied decision allowed: %s", source)
		}
	}
}
