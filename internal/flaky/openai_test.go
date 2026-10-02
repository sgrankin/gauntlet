package flaky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestOpenAIDecision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("wrong API request")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request["store"] != false || request["model"] != "chosen-model" {
			t.Error("wrong model or storage policy")
		}
		if strings.Contains(request["input"].(string), "PRIVATE_PREFIX") {
			t.Error("output was not bounded")
		}
		if request["tools"] != nil {
			t.Error("classifier can use tools")
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": `{"action":"retry","confidence":0.92,"reason":"connection reset"}`}}}}})
	}))
	defer server.Close()
	client := OpenAI{Model: "chosen-model", Token: "secret", BaseURL: server.URL + "/v1", MaxOutputBytes: 256}
	decision, err := client.Classify(context.Background(), core.CheckJob{Name: "test"}, core.CheckResult{Output: "PRIVATE_PREFIX" + strings.Repeat("x", 500)})
	if err != nil || decision.Action != "retry" || decision.Confidence != .92 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestOpenAIFailsClosed(t *testing.T) {
	for _, body := range []string{
		`{"status":"incomplete","output":[]}`,
		`{"status":"completed","output":[{"type":"message","content":[{"type":"refusal"}]}]}`,
		`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"action\":\"retry\",\"confidence\":2,\"reason\":\"bad\"}"}]}]}`,
		`{"status":"completed","output":[]}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(body)) }))
			defer server.Close()
			_, err := (OpenAI{BaseURL: server.URL, MaxOutputBytes: 256}).Classify(context.Background(), core.CheckJob{}, core.CheckResult{})
			if err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestDecisionRequiresExactSchema(t *testing.T) {
	for _, data := range []string{`{"action":"retry","reason":"missing confidence"}`, `{"action":"retry","confidence":1,"reason":"ok","command":"evil"}`, `{"action":"retry","confidence":1,"reason":"ok"} {}`, `{"action":"pass","confidence":1,"reason":"bad"}`} {
		if _, err := decodeDecision([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestOpenAINeverFollowsRedirects(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed credential-bearing redirect") }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	_, err := (OpenAI{BaseURL: source.URL, Token: "secret", Client: source.Client(), MaxOutputBytes: 256}).Classify(context.Background(), core.CheckJob{}, core.CheckResult{})
	if err == nil {
		t.Fatal("accepted redirect")
	}
}
