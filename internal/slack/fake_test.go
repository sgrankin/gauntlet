package slack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeSlack is an httptest-backed fake of just enough of Slack's socket-mode
// + Web API surface to exercise Slack end-to-end (docs/plans/phase23.md §5):
//
//   - POST /apps.connections.open -> returns a "url" pointing back at this
//     same server's websocket endpoint (verified against slack-go v0.27.0
//     source: this is the only place slack.OptionAPIURL needs to reach for
//     socket mode to work — see slack.go's package doc).
//   - GET  /link -> upgrades to a WebSocket, sends the socket-mode "hello"
//     envelope, and lets the test inject "events_api" envelopes
//     (reaction_added) via sendReaction.
//   - POST /chat.postMessage, /chat.update -> record the request (Slack's
//     Web API is form-encoded) and reply with minimal JSON {"ok":true,"ts":...}.
type fakeSlack struct {
	t   *testing.T
	srv *httptest.Server

	upgrader websocket.Upgrader

	connMu sync.Mutex
	conn   *websocket.Conn // the most recently dialed connection, if any
	connCh chan struct{}   // closed once, when the first conn arrives

	postsMu sync.Mutex
	posts   []postedMessage
	postCh  chan struct{} // closed and replaced on every recorded post

	seq atomic.Int64
}

// postedMessage is one recorded chat.postMessage/chat.update request.
type postedMessage struct {
	method   string // "chat.postMessage" | "chat.update"
	channel  string
	text     string
	ts       string // the ts in the response (server-assigned, or echoed back for update)
	threadTS string // "" for a root post
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{
		t:      t,
		connCh: make(chan struct{}),
		postCh: make(chan struct{}),
	}
	// slack-go's socket-mode dialer always sends "Origin: https://api.slack.com"
	// (socketmode/socket_mode_managed_conn.go), which gorilla's default
	// CheckOrigin rejects as cross-origin. This is a fake talking to
	// slack-go's real client, not a browser, so accept any origin.
	f.upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	mux := http.NewServeMux()
	mux.HandleFunc("/apps.connections.open", f.handleConnectionsOpen)
	mux.HandleFunc("/link", f.handleLink)
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) { f.handlePost(w, r, "chat.postMessage") })
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, r *http.Request) { f.handlePost(w, r, "chat.update") })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// apiURL is what New's Params.APIURL should be set to.
func (f *fakeSlack) apiURL() string { return f.srv.URL + "/" }

func (f *fakeSlack) handleConnectionsOpen(w http.ResponseWriter, r *http.Request) {
	wsURL := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/link"
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": wsURL}); err != nil {
		f.t.Errorf("encode apps.connections.open response: %v", err)
	}
}

func (f *fakeSlack) handleLink(w http.ResponseWriter, r *http.Request) {
	conn, err := f.upgrader.Upgrade(w, r, nil)
	if err != nil {
		f.t.Errorf("websocket upgrade: %v", err)
		return
	}

	// hello is sent before publishing conn, so there's no risk of a test
	// racing sendReaction against this write.
	hello := map[string]any{
		"type":            "hello",
		"num_connections": 1,
		"debug_info":      map[string]any{"host": "fake"},
		"connection_info": map[string]any{"app_id": "A_FAKE"},
	}
	if err := conn.WriteJSON(hello); err != nil {
		f.t.Errorf("write hello: %v", err)
		return
	}

	f.connMu.Lock()
	f.conn = conn
	wake := f.connCh
	f.connMu.Unlock()
	select {
	case <-wake:
		// already closed by an earlier connection; nothing to do
	default:
		close(wake)
	}

	// Drain and discard whatever the client sends back (acks, pings):
	// nothing here asserts on them, but the read loop must run so the
	// connection doesn't appear stalled.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (f *fakeSlack) handlePost(w http.ResponseWriter, r *http.Request, method string) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form for %s: %v", method, err)
		return
	}

	ts := r.Form.Get("ts")
	if ts == "" {
		ts = fmt.Sprintf("1700000000.%06d", f.seq.Add(1))
	}
	msg := postedMessage{
		method:   method,
		channel:  r.Form.Get("channel"),
		text:     r.Form.Get("text"),
		ts:       ts,
		threadTS: r.Form.Get("thread_ts"),
	}

	f.postsMu.Lock()
	f.posts = append(f.posts, msg)
	old := f.postCh
	f.postCh = make(chan struct{})
	f.postsMu.Unlock()
	close(old)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": msg.channel, "ts": msg.ts}); err != nil {
		f.t.Errorf("encode %s response: %v", method, err)
	}
}

// snapshotPosts returns every post recorded so far, in arrival order.
func (f *fakeSlack) snapshotPosts() []postedMessage {
	f.postsMu.Lock()
	defer f.postsMu.Unlock()
	out := make([]postedMessage, len(f.posts))
	copy(out, f.posts)
	return out
}

// waitForPosts blocks until at least n posts have been recorded, or fails
// the test after timeout. It synchronizes on postCh (closed on every
// recorded post) rather than a wall-clock sleep loop.
func (f *fakeSlack) waitForPosts(n int, timeout time.Duration) []postedMessage {
	f.t.Helper()
	deadline := time.After(timeout)
	for {
		f.postsMu.Lock()
		if len(f.posts) >= n {
			out := make([]postedMessage, len(f.posts))
			copy(out, f.posts)
			f.postsMu.Unlock()
			return out
		}
		wake := f.postCh
		f.postsMu.Unlock()

		select {
		case <-wake:
		case <-deadline:
			f.t.Fatalf("timed out waiting for %d posts (have %d)", n, len(f.snapshotPosts()))
		}
	}
}

// waitForConn blocks until the socket-mode client has dialed and been sent
// hello, or fails the test after timeout.
func (f *fakeSlack) waitForConn(timeout time.Duration) *websocket.Conn {
	f.t.Helper()
	f.connMu.Lock()
	wake := f.connCh
	f.connMu.Unlock()

	select {
	case <-wake:
	case <-time.After(timeout):
		f.t.Fatal("timed out waiting for a socket-mode connection")
	}

	f.connMu.Lock()
	defer f.connMu.Unlock()
	return f.conn
}

// sendReaction injects a reaction_added events_api envelope over the
// connection returned by waitForConn, as Slack would over the real
// socket-mode WebSocket.
func (f *fakeSlack) sendReaction(conn *websocket.Conn, user, reaction, itemTS string) {
	f.t.Helper()

	inner := map[string]any{
		"type":     "reaction_added",
		"user":     user,
		"reaction": reaction,
		"item":     map[string]any{"type": "message", "channel": "C_FAKE", "ts": itemTS},
		"event_ts": fmt.Sprintf("1700000000.%06d", f.seq.Add(1)),
	}
	payload := map[string]any{
		"type":       "event_callback",
		"token":      "fake-token",
		"team_id":    "T_FAKE",
		"api_app_id": "A_FAKE",
		"event":      inner,
		"event_id":   fmt.Sprintf("Ev%d", f.seq.Add(1)),
		"event_time": 1700000000,
	}
	envelope := map[string]any{
		"type":                     "events_api",
		"envelope_id":              fmt.Sprintf("env-%d", f.seq.Add(1)),
		"payload":                  payload,
		"accepts_response_payload": false,
	}

	f.connMu.Lock()
	defer f.connMu.Unlock()
	if err := conn.WriteJSON(envelope); err != nil {
		f.t.Fatalf("write reaction_added envelope: %v", err)
	}
}
