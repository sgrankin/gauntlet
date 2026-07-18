// Package ghauthtest provides a scripted fake GitHub App installation-token
// issuer for tests OUTSIDE internal/ghauth that need to exercise a real App
// JWT + token-mint round trip — gauntlet doctor's auth-mint probe tests
// (cmd/gauntlet/doctor_test.go) are the motivating case. internal/ghauth's
// own ghauth_test.go already has an equivalent fake issuer, but it is
// unexported and lives in a _test.go file, so it isn't importable past that
// package's boundary; this package exists specifically to make one
// reusable rather than have every other package grow its own copy. Same
// verification (a real RS256 signature and claim check against the
// caller's key), same scripted success/failure knob.
package ghauthtest

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issuer is a scripted GitHub "/app/installations/{id}/access_tokens"
// endpoint. It verifies every request's App JWT signature and claims
// (issuer, validity window) against its own key before answering, so a
// test using it proves a genuinely valid JWT was sent, not just present.
type Issuer struct {
	t              testing.TB
	key            *rsa.PrivateKey
	appID          int64
	installationID int64
	srv            *httptest.Server

	mu    sync.Mutex
	fail  bool // respond 401 instead of minting
	mints int32
}

// New starts a fake issuer scoped to appID/installationID, generating its
// own RSA key (exposed via Key) — the caller signs App JWTs with this same
// key (via ghauth.AppParams.Key) so the fake's verification actually means
// something. The server is closed automatically via t.Cleanup.
func New(t testing.TB, appID, installationID int64, key *rsa.PrivateKey) *Issuer {
	t.Helper()
	i := &Issuer{t: t, key: key, appID: appID, installationID: installationID}
	i.srv = httptest.NewServer(http.HandlerFunc(i.handle))
	t.Cleanup(i.srv.Close)
	return i
}

// URL is the fake issuer's base API URL — pass as ghauth.AppParams.APIURL.
func (i *Issuer) URL() string { return i.srv.URL }

// Client is the fake issuer's http.Client — pass as
// ghauth.AppParams.HTTPClient so the request actually reaches it (a plain
// http.Client would need no special transport for a plain httptest.Server,
// but this keeps the pairing explicit and matches ghauth_test.go's own
// pattern).
func (i *Issuer) Client() *http.Client { return i.srv.Client() }

// SetFail scripts every subsequent request to fail with 401, simulating a
// rejected mint (a revoked App install, a bad key on GitHub's side).
func (i *Issuer) SetFail(fail bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.fail = fail
}

// Mints returns how many tokens this issuer has minted so far.
func (i *Issuer) Mints() int32 { return atomic.LoadInt32(&i.mints) }

func (i *Issuer) handle(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	fail := i.fail
	i.mu.Unlock()

	wantPath := fmt.Sprintf("/app/installations/%d/access_tokens", i.installationID)
	if r.Method != http.MethodPost || r.URL.Path != wantPath {
		i.t.Errorf("ghauthtest: unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	i.verifyJWT(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if fail {
		http.Error(w, `{"message":"ghauthtest: issuer scripted to fail"}`, http.StatusUnauthorized)
		return
	}
	n := atomic.AddInt32(&i.mints, 1)
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"token":"ghs_FAKEMINT%d","expires_at":%q}`, n, time.Now().Add(time.Hour).Format(time.RFC3339))
}

// verifyJWT checks the RS256 signature and the claims GitHub requires,
// failing the test (not the request) on a mismatch — a test using this
// fake wants to know its own code produced a bad JWT, loudly.
func (i *Issuer) verifyJWT(jwt string) {
	i.t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		i.t.Errorf("ghauthtest: JWT has %d segments, want 3", len(parts))
		return
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		i.t.Errorf("ghauthtest: JWT signature decode: %v", err)
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&i.key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		i.t.Errorf("ghauthtest: JWT signature invalid: %v", err)
		return
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		i.t.Errorf("ghauthtest: JWT claims decode: %v", err)
		return
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		i.t.Errorf("ghauthtest: JWT claims parse: %v", err)
		return
	}
	if claims.Iss != strconv.FormatInt(i.appID, 10) {
		i.t.Errorf("ghauthtest: JWT iss = %q, want %q", claims.Iss, strconv.FormatInt(i.appID, 10))
	}
	now := time.Now().Unix()
	if claims.Iat > now || claims.Exp <= now {
		i.t.Errorf("ghauthtest: JWT window [%d,%d] does not contain now=%d", claims.Iat, claims.Exp, now)
	}
}
