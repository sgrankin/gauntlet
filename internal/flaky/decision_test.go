package flaky

import "testing"

func TestDecisionRequiresExactSchema(t *testing.T) {
	for _, data := range []string{`{"action":"retry","reason":"missing confidence"}`, `{"action":"retry","confidence":1,"reason":"ok","command":"evil"}`, `{"action":"retry","confidence":1,"reason":"ok"} {}`, `{"action":"pass","confidence":1,"reason":"bad"}`} {
		if _, err := decodeDecision([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}
