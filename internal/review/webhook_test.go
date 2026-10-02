package review

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func signedDelivery(body, secret, event string) *http.Request {
	r := httptest.NewRequest("POST", "/hooks/github", strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	r.Header.Set("X-GitHub-Event", event)
	return r
}

func TestWebhookAuthenticationAndRepository(t *testing.T) {
	valid := `{"repository":{"full_name":"acme/repo"}}`
	for _, tc := range []struct {
		name, body, event, signingSecret, configuredSecret string
		status                                             int
		wake                                               bool
	}{
		{"valid", valid, "issue_comment", "secret", "secret", 204, true},
		{"wrong secret", valid, "issue_comment", "other", "secret", 401, false},
		{"wrong repository", `{"repository":{"full_name":"other/repo"}}`, "push", "secret", "secret", 400, false},
		{"invalid JSON", "{", "push", "secret", "secret", 400, false},
		{"ping", "{}", "ping", "secret", "secret", 204, false},
		{"disabled", valid, "push", "", "", 503, false},
		{"oversized", strings.Repeat("x", (2<<20)+1), "push", "secret", "secret", 413, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGitHub(GitHubParams{Repo: "acme/repo"})
			woke := false
			w := httptest.NewRecorder()
			g.Webhook(tc.configuredSecret, func() { woke = true }).ServeHTTP(w, signedDelivery(tc.body, tc.signingSecret, tc.event))
			if w.Code != tc.status || woke != tc.wake {
				t.Fatalf("status=%d wake=%v, want %d %v", w.Code, woke, tc.status, tc.wake)
			}
		})
	}
}

func TestWebhookRefreshesCachedAdmission(t *testing.T) {
	f := newGitHubFixture(t)
	f.pulls[0].Stack = nil
	f.request(1, "@gauntlet merge")
	f.g.p.PollInterval = time.Hour
	if cs, err := f.g.Candidates(context.Background()); err != nil || len(cs) != 1 {
		t.Fatalf("initial: %+v %v", cs, err)
	}
	f.request(1, "@gauntlet cancel")
	wake := make(chan struct{}, 1)
	handler := f.g.Webhook("secret", func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	for range 2 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, signedDelivery(`{"repository":{"full_name":"acme/repo"}}`, "secret", "issue_comment"))
		if w.Code != 204 {
			t.Fatal(w.Code)
		}
	}
	if len(wake) != 1 {
		t.Fatal("duplicate deliveries not coalesced")
	}
	if cs, err := f.g.Candidates(context.Background()); err != nil || len(cs) != 0 {
		t.Fatalf("cancellation not refreshed: %+v %v", cs, err)
	}
}

func TestWebhookDuringPollDoesNotBlockOrLoseRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			close(started)
			<-release
		}
		json.NewEncoder(w).Encode([]pull{})
	}))
	defer srv.Close()
	g := NewGitHub(GitHubParams{Repo: "acme/repo", APIURL: srv.URL, Tokens: StaticToken("test"), PollInterval: time.Hour})
	done := make(chan error, 1)
	go func() { _, err := g.Candidates(context.Background()); done <- err }()
	<-started
	handled := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		g.Webhook("secret", nil).ServeHTTP(w, signedDelivery(`{"repository":{"full_name":"acme/repo"}}`, "secret", "pull_request"))
		handled <- w.Code
	}()
	select {
	case code := <-handled:
		if code != 204 {
			t.Error(code)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("webhook blocked behind API request")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := g.Candidates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatal("in-flight poll overwrote webhook refresh")
	}
}
