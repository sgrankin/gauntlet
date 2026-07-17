package ghauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testKey is generated once per process: 2048-bit keygen is the slow part
// of these tests and the key's identity doesn't matter.
var (
	testKeyOnce sync.Once
	testKeyVal  *rsa.PrivateKey
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		testKeyVal = k
	})
	return testKeyVal
}

// fakeIssuer is a scripted GitHub access_tokens endpoint. It verifies each
// request's App JWT signature and claims before answering, so the tests
// prove the JWT is genuinely valid, not just present.
type fakeIssuer struct {
	t   *testing.T
	key *rsa.PrivateKey
	now func() time.Time

	mu     sync.Mutex
	mints  int32
	fail   bool          // respond 401 instead of minting
	block  chan struct{} // when non-nil, requests wait here first
	expiry time.Duration // token lifetime from now(); default 1h
}

func (f *fakeIssuer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		block := f.block
		fail := f.fail
		exp := f.expiry
		f.mu.Unlock()
		if block != nil {
			<-block
		}
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/67890/access_tokens" {
			f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		f.verifyJWT(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if fail {
			http.Error(w, `{"message":"issuer says no"}`, http.StatusUnauthorized)
			return
		}
		n := atomic.AddInt32(&f.mints, 1)
		if exp == 0 {
			exp = time.Hour
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_FAKEMINT%d","expires_at":%q}`, n, f.now().Add(exp).Format(time.RFC3339))
	}
}

// verifyJWT checks the RS256 signature and the three claims GitHub
// requires.
func (f *fakeIssuer) verifyJWT(jwt string) {
	f.t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		f.t.Errorf("JWT has %d segments, want 3", len(parts))
		return
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		f.t.Errorf("JWT signature decode: %v", err)
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		f.t.Errorf("JWT signature invalid: %v", err)
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		f.t.Errorf("JWT claims decode: %v", err)
		return
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		f.t.Errorf("JWT claims parse: %v", err)
		return
	}
	if claims.Iss != "12345" {
		f.t.Errorf("JWT iss = %q, want \"12345\"", claims.Iss)
	}
	now := f.now().Unix()
	if claims.Iat > now || claims.Exp <= now {
		f.t.Errorf("JWT window [%d,%d] does not contain now=%d", claims.Iat, claims.Exp, now)
	}
}

// appHarness wires an App to a fake issuer and a movable clock.
type appHarness struct {
	app    *App
	issuer *fakeIssuer

	mu  sync.Mutex
	now time.Time
}

func newAppHarness(t *testing.T) *appHarness {
	t.Helper()
	h := &appHarness{now: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)}
	h.issuer = &fakeIssuer{t: t, key: testKey(t), now: h.clock}
	srv := httptest.NewServer(h.issuer.handler())
	t.Cleanup(srv.Close)
	app, err := NewApp(AppParams{
		AppID:          12345,
		InstallationID: 67890,
		Key:            testKey(t),
		APIURL:         srv.URL,
		HTTPClient:     srv.Client(),
		Now:            h.clock,
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	h.app = app
	return h
}

func (h *appHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *appHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func TestApp_LazyMintAndCache(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()

	if n := atomic.LoadInt32(&h.issuer.mints); n != 0 {
		t.Fatalf("mints before first Token = %d, want 0 (lazy)", n)
	}
	tok1, err := h.app.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok1 != "ghs_FAKEMINT1" {
		t.Fatalf("token = %q", tok1)
	}
	// Within the validity window: cached, no second mint.
	h.advance(30 * time.Minute)
	tok2, err := h.app.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok2 != tok1 {
		t.Fatalf("token changed inside validity: %q -> %q", tok1, tok2)
	}
	if n := atomic.LoadInt32(&h.issuer.mints); n != 1 {
		t.Fatalf("mints = %d, want 1", n)
	}
}

func TestApp_ProactiveRefreshBeforeExpiry(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	if _, err := h.app.Token(ctx); err != nil {
		t.Fatalf("Token: %v", err)
	}
	// 59m: inside the 2m refresh window of the 60m expiry — must re-mint
	// even though the token is not yet expired.
	h.advance(59 * time.Minute)
	tok, err := h.app.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_FAKEMINT2" {
		t.Fatalf("token = %q, want the proactively refreshed ghs_FAKEMINT2", tok)
	}
}

func TestApp_ConcurrentCallersShareOneMint(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	gate := make(chan struct{})
	h.issuer.mu.Lock()
	h.issuer.block = gate
	h.issuer.mu.Unlock()

	const callers = 8
	tokens := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = h.app.Token(ctx)
		}(i)
	}
	// Let the callers pile up behind the single in-flight mint, then
	// release it. (No count-based rendezvous is available; the assertion
	// below — one mint total — is what actually proves the sharing.)
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if tokens[i] != "ghs_FAKEMINT1" {
			t.Fatalf("caller %d got %q, want the one shared mint", i, tokens[i])
		}
	}
	if n := atomic.LoadInt32(&h.issuer.mints); n != 1 {
		t.Fatalf("mints = %d, want 1 (singleflight)", n)
	}
}

func TestApp_InvalidateForcesOneFreshMint(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	tok1, err := h.app.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	h.app.Invalidate(tok1)
	tok2, err := h.app.Token(ctx)
	if err != nil {
		t.Fatalf("Token after Invalidate: %v", err)
	}
	if tok2 == tok1 {
		t.Fatalf("token unchanged after Invalidate")
	}

	// A stale Invalidate (the OLD token) must not kill the fresh one.
	h.app.Invalidate(tok1)
	tok3, err := h.app.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok3 != tok2 {
		t.Fatalf("stale Invalidate replaced a fresh token: %q -> %q", tok2, tok3)
	}
	if n := atomic.LoadInt32(&h.issuer.mints); n != 2 {
		t.Fatalf("mints = %d, want 2", n)
	}
}

func TestApp_IssuerFailureIsOperationalAndRecovers(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	h.issuer.mu.Lock()
	h.issuer.fail = true
	h.issuer.mu.Unlock()

	if _, err := h.app.Token(ctx); err == nil {
		t.Fatal("Token = nil error while the issuer refuses, want an operational error")
	}

	h.issuer.mu.Lock()
	h.issuer.fail = false
	h.issuer.mu.Unlock()
	tok, err := h.app.Token(ctx)
	if err != nil {
		t.Fatalf("Token after issuer recovery: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token after recovery")
	}
}

func TestApp_WaitersShareMintFailure(t *testing.T) {
	h := newAppHarness(t)
	ctx := context.Background()
	gate := make(chan struct{})
	h.issuer.mu.Lock()
	h.issuer.fail = true
	h.issuer.block = gate
	h.issuer.mu.Unlock()

	const callers = 4
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = h.app.Token(ctx)
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d: nil error, want the shared mint failure", i)
		}
	}
}

func TestStatic_PassthroughAndNoopInvalidate(t *testing.T) {
	s := Static("ghp_FAKESTATIC")
	tok, err := s.Token(context.Background())
	if err != nil || tok != "ghp_FAKESTATIC" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	s.Invalidate("ghp_FAKESTATIC")
	tok, _ = s.Token(context.Background())
	if tok != "ghp_FAKESTATIC" {
		t.Fatalf("static token changed after Invalidate: %q", tok)
	}
}

func TestLoadPrivateKey(t *testing.T) {
	key := testKey(t)
	dir := t.TempDir()

	writeKey := func(name string, mode os.FileMode, der []byte, pemType string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		buf := pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: der})
		if err := os.WriteFile(path, buf, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	pkcs1 := writeKey("pkcs1.pem", 0o600, x509.MarshalPKCS1PrivateKey(key), "RSA PRIVATE KEY")
	if _, err := LoadPrivateKey(pkcs1); err != nil {
		t.Errorf("PKCS#1 0600: %v", err)
	}

	pkcs8der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := writeKey("pkcs8.pem", 0o600, pkcs8der, "PRIVATE KEY")
	if _, err := LoadPrivateKey(pkcs8); err != nil {
		t.Errorf("PKCS#8 0600: %v", err)
	}

	loose := writeKey("loose.pem", 0o644, x509.MarshalPKCS1PrivateKey(key), "RSA PRIVATE KEY")
	if _, err := LoadPrivateKey(loose); err == nil || !strings.Contains(err.Error(), "group/other-accessible") {
		t.Errorf("world-readable key: err = %v, want a mode rejection", err)
	}

	if _, err := LoadPrivateKey(filepath.Join(dir, "absent.pem")); err == nil {
		t.Error("absent key file: nil error")
	}

	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKey(garbage); err == nil || !strings.Contains(err.Error(), "no PEM block") {
		t.Errorf("garbage key: err = %v, want a PEM rejection", err)
	}
}
