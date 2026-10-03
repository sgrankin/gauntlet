# GitHub credentials

Choose a static API token plus ordinary Git credentials, or a GitHub App that
authenticates both API and Git. Credentials must belong to the daemon service
user, not only your interactive shell.

## GitHub App

```kdl
github "acme/widgets" {
    auth "app" {
        app-id 12345
        installation-id 67890
        private-key-file "/run/credentials/gauntlet-app.pem"
    }
}
```

1. Register/install the App on the target repository and provision its private
   key with `0400` or `0600` permissions.
2. Use a credential-free HTTPS `remote` matching that repository and host.
3. Grant permissions from the table below for enabled features.
4. Run `gauntlet doctor`; it mints a real short-lived installation token to
   verify authentication. Token refresh is automatic; key rotation needs restart.

In Kubernetes, projected keys need `defaultMode: 0400`; avoid an `fsGroup` that
makes them group-readable. A read-only systemd credential or a private init-container
copy also works. Ensure `TMPDIR` is executable for the temporary Git askpass helper.

## Static token and Git transport

Configure `token-env` (default `GITHUB_TOKEN`) with a fine-grained PAT scoped to
this repository. It authenticates GitHub API calls, not Git fetch/push.
Use either:

- SSH remote: a writable deploy key or account key, with trusted host keys.
- HTTPS remote: a credential helper available to the service user.

Avoid storing a token in the remote URL: repository configuration is accessible
to candidate commands. The API token must be populated at startup when GitHub
is configured. See [forge settings](../reference/github.md).

## Repository permissions

| Feature | Required access |
|---|---|
| Fetch/push, target publication, trial refs, receipt notes | Contents read/write for App or HTTPS token transport; equivalent Git permissions for SSH. |
| Status reporting | Commit statuses read/write. |
| PR intake and completion | Pull requests read/write, issues read/write for comments, checks/statuses read, and collaborator permission access. |
| Changes to `.github/workflows/` | Workflows write for the pushing token. |
| Policy team lookups | Permission to read the configured organization team memberships. |

Protect the target so the queue can push and other writers cannot race it.
Branch rules must permit the queue's direct tested-commit push. Gauntlet does
not delegate commit creation to GitHub's merge button.

## Verify

Push a disposable candidate and confirm `gauntlet/<target>` status transitions.
Without trial refs the status names the source SHA; with them it names the tested
commit. For PR intake, run `@gauntlet check` as an authorized user, request a merge,
and verify the bot's commit link and closed PR. Signed webhook registration is
separate; see [GitHub setup](github.md#polling-and-optional-webhooks).
