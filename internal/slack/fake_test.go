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

// fakeBotUserID is the user id this fake reports for auth.test — every
// chat.postMessage/chat.update in these tests is "posted by" this id
// (recorded in msgs, below), so handleForeignReaction's authorship check
// (Slack.isOwnMessage) has something real to compare against.
const fakeBotUserID = "UBOTFAKE"

// fakeBotID is the companion B… bot id (auth.test's bot_id). The fake
// serves bot-posted messages back through conversations.history in the REAL
// bot_message shape — bot_id set, NO top-level user — because that is what
// live Slack does and trusting the wrong field here is exactly the
// green-in-CI/broken-live trap this suite exists to prevent (fresh-context
// review of the metadata-ownership commit, finding 1).
const fakeBotID = "BBOTFAKE"

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
//     Web API is form-encoded), remember its current (ts, text, metadata)
//     state for conversations.history to later serve, and reply with
//     minimal JSON {"ok":true,"ts":...}.
//   - POST /auth.test -> reports fakeBotUserID as this fake's own bot user
//     id (Slack.Run fetches this once, at
//     startup, to check message authorship).
//   - POST /conversations.history -> serves back the one message matching
//     the "latest"/"oldest" ts (Slack.handleForeignReaction always fetches
//     exactly one message this way), metadata included — the fetch path a
//     reaction takes once its run has already terminated and the in-memory
//     roots map has forgotten it.
//   - POST /reactions.add -> records the (channel, ts, emoji) triple so
//     tests can assert an inbound reaction was acknowledged.
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

	// msgs holds the CURRENT state of every message known to this fake,
	// keyed by ts — updated on every chat.postMessage/chat.update, and
	// directly seedable via injectForeignMessage for a message this bot
	// never posted. conversations.history (handleConversationsHistory)
	// serves from here, not from posts: posts is an append-only log for
	// arrival-order assertions, but a fetch-by-ts always wants latest state
	// (a chat.update must be reflected, matching the real API).
	msgsMu sync.Mutex
	msgs   map[string]postedMessage

	reactionsMu sync.Mutex
	reactions   []reactionAdd
	reactionCh  chan struct{} // closed and replaced on every recorded reactions.add

	seq atomic.Int64
}

// postedMessage is one recorded chat.postMessage/chat.update request (or,
// via injectForeignMessage, a synthetic message this test seeded directly
// without going through either call).
type postedMessage struct {
	method   string // "chat.postMessage" | "chat.update" | "" for an injected message
	channel  string
	text     string
	ts       string // the ts in the response (server-assigned, or echoed back for update)
	threadTS string // "" for a root post
	user     string // the posting user id ("" for a message with no known author)
	botID    string // the posting app's bot id ("" for a human message)

	// metadataSet is true iff this request carried a "metadata" form field
	// at all (chat.postMessage/chat.update calls made without
	// MsgOptionMetadata never do) — needed because eventType=="" is
	// ambiguous between "no metadata sent" and "metadata sent with an empty
	// event_type"; tests asserting metadata shape check this first.
	metadataSet bool
	eventType   string
	payload     map[string]any
}

// reactionAdd is one recorded reactions.add request.
type reactionAdd struct {
	channel string
	ts      string
	name    string
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{
		t:          t,
		connCh:     make(chan struct{}),
		postCh:     make(chan struct{}),
		msgs:       make(map[string]postedMessage),
		reactionCh: make(chan struct{}),
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
	mux.HandleFunc("/auth.test", f.handleAuthTest)
	mux.HandleFunc("/conversations.history", f.handleConversationsHistory)
	mux.HandleFunc("/reactions.add", f.handleReactionsAdd)
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
		// Real bot_message shape: identity in bot_id, top-level user
		// deliberately empty — live conversations.history does not reliably
		// populate user for bot posts, so the fake must not either (see
		// fakeBotID).
		user:  "",
		botID: fakeBotID,
	}

	if rawMeta := r.Form.Get("metadata"); rawMeta != "" {
		var decoded struct {
			EventType    string         `json:"event_type"`
			EventPayload map[string]any `json:"event_payload"`
		}
		if err := json.Unmarshal([]byte(rawMeta), &decoded); err != nil {
			f.t.Errorf("decode metadata form field for %s: %v", method, err)
		} else {
			msg.metadataSet = true
			msg.eventType = decoded.EventType
			msg.payload = decoded.EventPayload
		}
	}

	f.postsMu.Lock()
	f.posts = append(f.posts, msg)
	old := f.postCh
	f.postCh = make(chan struct{})
	f.postsMu.Unlock()
	close(old)

	// conversations.history always serves the CURRENT state for a ts: a
	// chat.update that omits metadata (or overwrites it) must be reflected
	// here exactly like the real API — so a threaded reply/notice (no ts
	// form field at all, hence a fresh server-assigned ts, never re-used by
	// a later update) never clobbers its root's own entry.
	f.msgsMu.Lock()
	f.msgs[ts] = msg
	f.msgsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": msg.channel, "ts": msg.ts}); err != nil {
		f.t.Errorf("encode %s response: %v", method, err)
	}
}

// handleAuthTest serves auth.test: every call reports fakeBotUserID as this
// fake's own bot user id, so Slack.Run's startup fetch has a
// stable identity for isOwnMessage to compare against.
func (f *fakeSlack) handleAuthTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "user_id": fakeBotUserID, "bot_id": fakeBotID}); err != nil {
		f.t.Errorf("encode auth.test response: %v", err)
	}
}

// handleConversationsHistory serves conversations.history: Slack.handleForeignReaction
// always requests exactly one message (latest==oldest==ts, inclusive, limit
// 1 — the documented way to fetch a single message by ts), so this looks the
// ts up in f.msgs (current state, including any chat.update / injected
// message) and returns it alone, or an empty list if unknown.
func (f *fakeSlack) handleConversationsHistory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form for conversations.history: %v", err)
		return
	}
	ts := r.Form.Get("latest")
	if ts == "" {
		ts = r.Form.Get("oldest")
	}

	f.msgsMu.Lock()
	msg, ok := f.msgs[ts]
	f.msgsMu.Unlock()

	messages := []map[string]any{}
	if ok {
		m := map[string]any{
			"type": "message",
			"ts":   msg.ts,
			"text": msg.text,
			"metadata": map[string]any{
				"event_type":    msg.eventType,
				"event_payload": msg.payload,
			},
		}
		// Mirror the real API's authorship shape: a human message carries
		// user; a bot message carries subtype/bot_id and may omit user
		// entirely (see fakeBotID).
		if msg.user != "" {
			m["user"] = msg.user
		}
		if msg.botID != "" {
			m["subtype"] = "bot_message"
			m["bot_id"] = msg.botID
		}
		messages = append(messages, m)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": messages}); err != nil {
		f.t.Errorf("encode conversations.history response: %v", err)
	}
}

// handleReactionsAdd serves reactions.add: records the (channel, ts, emoji)
// triple so tests can assert an inbound reaction was acknowledged (design
// §3), and wakes anything blocked in waitForReactions.
func (f *fakeSlack) handleReactionsAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form for reactions.add: %v", err)
		return
	}
	ra := reactionAdd{
		channel: r.Form.Get("channel"),
		ts:      r.Form.Get("timestamp"),
		name:    r.Form.Get("name"),
	}

	f.reactionsMu.Lock()
	f.reactions = append(f.reactions, ra)
	old := f.reactionCh
	f.reactionCh = make(chan struct{})
	f.reactionsMu.Unlock()
	close(old)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true}); err != nil {
		f.t.Errorf("encode reactions.add response: %v", err)
	}
}

// injectForeignMessage seeds f.msgs with a message this bot never posted
// (no matching entry ever went through handlePost) — the shape a genuinely
// foreign HUMAN message has when conversations.history serves it back:
// authorship in user, no bot_id. For a foreign app's message, use
// injectForeignBotMessage.
func (f *fakeSlack) injectForeignMessage(ts, user, eventType string, payload map[string]any) {
	f.msgsMu.Lock()
	defer f.msgsMu.Unlock()
	f.msgs[ts] = postedMessage{
		ts:          ts,
		user:        user,
		metadataSet: eventType != "" || payload != nil,
		eventType:   eventType,
		payload:     payload,
	}
}

// injectForeignBotMessage seeds f.msgs with another app's bot message:
// bot_message shape (bot_id set, no user) but a bot_id that is not
// fakeBotID. This is the spoofing vector the authorship check exists for —
// any app can stamp gauntlet-lookalike metadata on its own messages.
func (f *fakeSlack) injectForeignBotMessage(ts, botID, eventType string, payload map[string]any) {
	f.msgsMu.Lock()
	defer f.msgsMu.Unlock()
	f.msgs[ts] = postedMessage{
		ts:          ts,
		botID:       botID,
		metadataSet: eventType != "" || payload != nil,
		eventType:   eventType,
		payload:     payload,
	}
}

// snapshotReactions returns every reactions.add call recorded so far, in
// arrival order.
func (f *fakeSlack) snapshotReactions() []reactionAdd {
	f.reactionsMu.Lock()
	defer f.reactionsMu.Unlock()
	out := make([]reactionAdd, len(f.reactions))
	copy(out, f.reactions)
	return out
}

// waitForReactions blocks until at least n reactions.add calls have been
// recorded, or fails the test after timeout — mirroring waitForPosts.
func (f *fakeSlack) waitForReactions(n int, timeout time.Duration) []reactionAdd {
	f.t.Helper()
	deadline := time.After(timeout)
	for {
		f.reactionsMu.Lock()
		if len(f.reactions) >= n {
			out := make([]reactionAdd, len(f.reactions))
			copy(out, f.reactions)
			f.reactionsMu.Unlock()
			return out
		}
		wake := f.reactionCh
		f.reactionsMu.Unlock()

		select {
		case <-wake:
		case <-deadline:
			f.t.Fatalf("timed out waiting for %d reactions.add calls (have %d)", n, len(f.snapshotReactions()))
		}
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
