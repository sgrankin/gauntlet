// GitHub authentication wiring (issue #6): cmd is the sole place the
// config block, the ghauth provider, gitx, and ghstatus meet — the same
// bridging rule as channels.go.
package main

import (
	"fmt"
	"net/url"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/ghauth"
	"github.com/sgrankin/gauntlet/internal/gitx"
)

// buildAppTokens constructs the GitHub App installation-token provider
// when cfg selects `auth "app"`, or nil in static-token/disabled mode.
// The private key is loaded (and its file mode validated) here, at
// startup, so a bad key path fails the daemon loudly before any queue
// work — key rotation is a restart, token refresh is not (issue #6).
func buildAppTokens(cfg *config.Daemon) (*ghauth.App, error) {
	a := cfg.GitHub.Auth
	if a == nil {
		return nil, nil
	}
	key, err := ghauth.LoadPrivateKey(a.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("github: auth \"app\": %w", err)
	}
	app, err := ghauth.NewApp(ghauth.AppParams{
		AppID:          a.AppID,
		InstallationID: a.InstallationID,
		Key:            key,
		APIURL:         cfg.GitHub.APIURL,
	})
	if err != nil {
		return nil, fmt.Errorf("github: auth \"app\": %w", err)
	}
	return app, nil
}

// gitAuthOptions returns the gitx options app mode needs: the shared
// provider, scoped to the configured remote's host (config.validate
// already proved the remote is HTTPS and canonicalizes to the github
// block's host and owner/repo). Empty in static mode — git keeps ambient
// auth exactly as before.
func gitAuthOptions(cfg *config.Daemon, app *ghauth.App) ([]gitx.Option, error) {
	if app == nil {
		return nil, nil
	}
	u, err := url.Parse(cfg.Remote)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("github: auth \"app\": remote %q does not parse as a URL: %v", cfg.Remote, err)
	}
	return []gitx.Option{gitx.WithTokenSource(app, u.Host)}, nil
}
