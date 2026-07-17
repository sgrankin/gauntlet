package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TokenSource supplies a current HTTPS credential for remote operations
// (fetch, push). Implemented by internal/ghauth (App installation tokens
// and static PATs alike); gitx defines its own copy of the two-method
// shape so the dependency points the right way.
type TokenSource interface {
	// Token returns a currently valid token. Called once per remote
	// operation — the provider owns caching and refresh.
	Token(ctx context.Context) (string, error)
	// Invalidate reports that token was rejected by the remote, so the
	// provider can drop it from its cache before the caller's single
	// retry.
	Invalidate(token string)
}

// Option configures New.
type Option func(*Repo)

// WithTokenSource makes every remote operation authenticate via ts,
// scoped to host (the remote URL's host — a credential is only ever
// answered for prompts naming it, so a redirect to any other host gets
// nothing). The token travels exclusively through an ephemeral
// GIT_ASKPASS helper plus the git subprocess's environment: never argv
// (ps-visible), never the persistent remote URL (readable by checks via
// the mounted GAUNTLET_GIT_DIR — see the DESIGN.md ledger), never git
// config.
func WithTokenSource(ts TokenSource, host string) Option {
	return func(r *Repo) {
		r.tokens = ts
		r.authHost = host
	}
}

// askpassScript is the ephemeral GIT_ASKPASS helper. It contains NO
// secret — the credential rides in the git subprocess's environment and
// is read back here — so the file itself needs only lifecycle hygiene
// (0700, removed after the operation), not secret handling. The first
// case scopes the credential: git's prompt always names the host it is
// asking for ("Password for 'https://x-access-token@github.com': "), so
// a prompt for any host other than the configured one — an unexpected
// redirect (git's http.followRedirects default chases them on the
// initial request), a rogue submodule URL — gets a hard refusal instead
// of a forwarded token.
//
// The match is ANCHORED on the delimiters that bound a host inside git's
// prompt URL — "://" or "@" before, "'" (prompt quote) or "/" (path,
// when credential.useHttpPath is set) after — never a bare substring: a
// substring match would hand the token to "github.com.evil.example" or
// "evil-github.com", which both CONTAIN "github.com" (review finding on
// the first cut of this file, empirically reproduced). A hostname can
// contain neither "/" nor "'", so the anchors can't be smuggled into a
// hostile host.
const askpassScript = `#!/bin/sh
case "$1" in
*"://$GAUNTLET_ASKPASS_HOST'"* | *"@$GAUNTLET_ASKPASS_HOST'"* | \
*"://$GAUNTLET_ASKPASS_HOST/"* | *"@$GAUNTLET_ASKPASS_HOST/"*) ;;
*) exit 1 ;;
esac
case "$1" in
[Uu]sername*) printf '%s\n' "$GAUNTLET_ASKPASS_USER" ;;
*) printf '%s\n' "$GAUNTLET_ASKPASS_PASS" ;;
esac
`

// runRemote runs a git command that talks to origin. Without a
// TokenSource it is plain run (ambient auth — SSH, credential helpers —
// exactly as before). With one, the operation authenticates via askpass,
// and a clear credential rejection triggers exactly one
// invalidate-and-retry with a freshly minted token: the
// expired-mid-operation case refresh exists for. Anything else — 403
// permission denial, network failure, non-fast-forward — is returned
// as-is, never retried here (the reconcile loop owns coarse retry
// semantics).
func (r *Repo) runRemote(ctx context.Context, args ...string) (string, error) {
	if r.tokens == nil {
		return r.run(ctx, args...)
	}
	out, tok, err := r.runAuthed(ctx, args...)
	if err != nil && isAuthRejection(err) {
		r.tokens.Invalidate(tok)
		out, _, err = r.runAuthed(ctx, args...)
	}
	return out, err
}

// runAuthed runs one git command with an ephemeral askpass credential.
// The helper lives in a fresh 0700 directory removed on every path out —
// success, failure, and cancellation alike — via the defer.
func (r *Repo) runAuthed(ctx context.Context, args ...string) (out, token string, err error) {
	tok, err := r.tokens.Token(ctx)
	if err != nil {
		return "", "", fmt.Errorf("credential: %w", err)
	}
	dir, err := os.MkdirTemp("", "gauntlet-askpass-")
	if err != nil {
		return "", tok, fmt.Errorf("askpass dir: %w", err)
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(script, []byte(askpassScript), 0o700); err != nil {
		return "", tok, fmt.Errorf("askpass helper: %w", err)
	}
	env := append(os.Environ(),
		"GIT_ASKPASS="+script,
		"GIT_TERMINAL_PROMPT=0",
		"GAUNTLET_ASKPASS_HOST="+r.authHost,
		"GAUNTLET_ASKPASS_USER=x-access-token",
		"GAUNTLET_ASKPASS_PASS="+tok,
		// isAuthRejection (and isStaleLease) classify by matching git's
		// English messages, which git localizes: an operator locale with
		// git-l10n installed would silently defeat the retry contract.
		// Appended last, so it wins any earlier LC_ALL (os/exec keeps
		// the last duplicate).
		"LC_ALL=C",
	)
	out, err = runGitEnv(ctx, r.dir, nil, env, args...)
	return out, tok, err
}

// isAuthRejection reports whether err is git's clear credential-rejected
// failure (HTTP 401 surfaces as "Authentication failed for '<url>'").
// Deliberately narrow: a 403 ("The requested URL returned error: 403")
// means the credential IS valid but lacks permission — retrying with a
// fresh token cannot fix that, and the issue's contract forbids retrying
// arbitrary authorization failures.
func isAuthRejection(err error) bool {
	var ge *gitError
	return errors.As(err, &ge) && strings.Contains(ge.stderr, "Authentication failed")
}
