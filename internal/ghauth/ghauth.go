// Package ghauth supplies GitHub credentials to the rest of the daemon as
// a refreshable token source: gitx (fetch/push over HTTPS) and ghstatus
// (commit-status REST posts) both consume the same provider, so refresh,
// redaction, and failure semantics live here exactly once (issue #6).
//
// Two implementations:
//
//   - Static: today's PAT-from-environment mode, unchanged semantics — the
//     token never expires and never refreshes.
//   - App: GitHub App installation tokens. A short-lived RS256 App JWT
//     (stdlib crypto; no JWT dependency for three fixed claims) is
//     exchanged for an installation token on demand, cached until a safety
//     window before expiry, refreshed without restart, and minted at most
//     once at a time however many callers race (singleflight).
//
// A token is handed out as a value, never stored anywhere persistent by
// this package; callers must keep it out of argv, remote URLs, config,
// logs, and anything a candidate command can read (see the Invalidate
// contract for the one sanctioned reaction to a rejected token).
package ghauth

import (
	"bytes"
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
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Static is a fixed personal-access-token source: Token always returns the
// same value and Invalidate is a no-op (a PAT that stops working needs an
// operator, not a refresh).
type Static string

// Token returns the static token.
func (s Static) Token(ctx context.Context) (string, error) { return string(s), nil }

// Invalidate is a no-op for a static token.
func (s Static) Invalidate(token string) {}

// refreshWindow is how long before expiry a cached installation token is
// treated as already stale. GitHub installation tokens live one hour; two
// minutes absorbs clock skew between us and GitHub plus the longest
// plausible push/status round-trip started just before the boundary.
const refreshWindow = 2 * time.Minute

// jwtLifetime is the App JWT's validity (GitHub caps it at 10 minutes;
// staying well under the cap tolerates daemon-side clock skew ahead of
// GitHub), and jwtBackdate guards against skew behind GitHub.
const (
	jwtLifetime = 5 * time.Minute
	jwtBackdate = time.Minute
)

// mintTimeout bounds one access-token exchange so a wedged GitHub cannot
// hold every Token caller (and with them the reconcile tick) hostage.
const mintTimeout = 30 * time.Second

// AppParams configures an App token source.
type AppParams struct {
	// AppID and InstallationID identify the GitHub App and its
	// installation on the configured repository.
	AppID          int64
	InstallationID int64

	// Key signs App JWTs. Load it with LoadPrivateKey (file mode
	// validation + PEM parsing) or supply one directly in tests.
	Key *rsa.PrivateKey

	// APIURL is the GitHub REST API base URL (e.g.
	// "https://api.github.com", or "https://ghe.example.com/api/v3").
	APIURL string

	// HTTPClient overrides the exchange client; nil uses a default with
	// mintTimeout. Tests point this at a fake issuer.
	HTTPClient *http.Client

	// Now overrides the clock; nil uses time.Now. Tests drive refresh
	// behavior through this, never wall-clock sleeps.
	Now func() time.Time
}

// App mints and caches GitHub App installation tokens.
type App struct {
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	apiURL         string
	client         *http.Client
	now            func() time.Time

	mu      sync.Mutex
	token   string
	expiry  time.Time
	minting *mint // non-nil while one exchange is in flight
}

// mint is one in-flight token exchange; concurrent Token callers wait on
// done and share its outcome instead of stampeding the issuer.
type mint struct {
	done   chan struct{}
	token  string
	expiry time.Time
	err    error
}

// NewApp validates p and returns an App source. No token is minted here —
// the first Token call mints lazily, so a daemon can start while GitHub is
// down and recover on its own.
func NewApp(p AppParams) (*App, error) {
	if p.AppID <= 0 {
		return nil, fmt.Errorf("ghauth: app-id must be positive, got %d", p.AppID)
	}
	if p.InstallationID <= 0 {
		return nil, fmt.Errorf("ghauth: installation-id must be positive, got %d", p.InstallationID)
	}
	if p.Key == nil {
		return nil, fmt.Errorf("ghauth: private key is required")
	}
	if p.APIURL == "" {
		return nil, fmt.Errorf("ghauth: api url is required")
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: mintTimeout}
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	return &App{
		appID:          p.AppID,
		installationID: p.InstallationID,
		key:            p.Key,
		apiURL:         p.APIURL,
		client:         client,
		now:            now,
	}, nil
}

// Token returns a currently valid installation token, minting or
// refreshing if the cache is empty, invalidated, or within refreshWindow
// of expiry. Concurrent callers share one exchange. Errors are operational
// (issuer unreachable, key rejected) — callers surface them as
// infrastructure failures, never as a bad candidate.
func (a *App) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	for {
		if a.token != "" && a.now().Add(refreshWindow).Before(a.expiry) {
			t := a.token
			a.mu.Unlock()
			return t, nil
		}
		if a.minting == nil {
			break // this caller performs the mint
		}
		m := a.minting
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-m.done:
		}
		if m.err == nil {
			return m.token, nil
		}
		// The shared mint failed. Report the shared error rather than
		// looping into a fresh mint: every waiter failing fast beats a
		// serial retry stampede against an issuer that just refused.
		return "", m.err
	}

	m := &mint{done: make(chan struct{})}
	a.minting = m
	a.mu.Unlock()

	// The exchange runs detached from the initiating caller's
	// cancellation (the classic hand-rolled-singleflight footgun): its
	// result is shared by every waiter, so one caller's dying ctx must
	// not manufacture a "credential failure" for the healthy ones.
	// exchange's own mintTimeout keeps it bounded regardless.
	m.token, m.expiry, m.err = a.exchange(context.WithoutCancel(ctx))

	a.mu.Lock()
	if m.err == nil {
		a.token, a.expiry = m.token, m.expiry
	}
	a.minting = nil
	a.mu.Unlock()
	close(m.done)

	if m.err != nil {
		return "", m.err
	}
	return m.token, nil
}

// Invalidate drops token from the cache if it is still the cached one, so
// the next Token call mints fresh. Callers use this exactly once per
// clearly credential-rejected response (401 from GitHub, git
// authentication failure) before a single retry — never in a loop. The
// equality guard makes a stale caller's Invalidate of an already-replaced
// token a no-op instead of a fresh token's death.
func (a *App) Invalidate(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == token {
		a.token, a.expiry = "", time.Time{}
	}
}

// exchange mints an App JWT and trades it for an installation token.
func (a *App) exchange(ctx context.Context) (string, time.Time, error) {
	jwt, err := a.appJWT()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("ghauth: sign app jwt: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiURL, a.installationID)
	ctx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("ghauth: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("ghauth: mint installation token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusCreated {
		// The response body of a FAILED mint carries GitHub's error
		// message, never a token; the JWT lives only in the request
		// header. Safe to quote, bounded above.
		return "", time.Time{}, fmt.Errorf("ghauth: mint installation token: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", time.Time{}, fmt.Errorf("ghauth: parse token response: %w", err)
	}
	if payload.Token == "" || payload.ExpiresAt.IsZero() {
		return "", time.Time{}, fmt.Errorf("ghauth: token response missing token or expires_at")
	}
	return payload.Token, payload.ExpiresAt, nil
}

// appJWT signs the three-claim RS256 App JWT GitHub requires. Done with
// stdlib crypto on purpose: a fixed header and three integer/string claims
// don't justify a JWT dependency.
func (a *App) appJWT() (string, error) {
	now := a.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": strconv.FormatInt(a.appID, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// LoadPrivateKey reads and parses an RSA private key PEM (PKCS#1 or
// PKCS#8) from path, rejecting group/other-accessible files: the key IS
// the App's identity, and a merge-queue daemon host runs candidate code —
// the file mode is the one line of defense config can check at startup.
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("ghauth: private key %s: %w", path, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf("ghauth: private key %s: mode %04o is group/other-accessible; chmod 0600 it", path, mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ghauth: private key %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("ghauth: private key %s: no PEM block found", path)
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ghauth: private key %s: parse: %w", path, err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("ghauth: private key %s: not an RSA key (GitHub App keys are RSA)", path)
	}
	return key, nil
}
