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

## Reserve the target for Gauntlet

Use a GitHub App as the queue's publishing identity. An active branch ruleset
can prevent regular merges and direct pushes while permitting Gauntlet's tested
history:

1. In **Settings → Rules → Rulesets**, create a branch ruleset targeting the
   real branch configured in Gauntlet, such as `main` or `master`.
2. Enable **Restrict updates**. Only bypass actors can update that branch,
   including through the merge button.
3. Add the installed Gauntlet App to the bypass list with **Always** access.
   PR-only bypass would not permit its direct Git push. Keep other writers and
   broad administrator roles off that list if the queue must own all updates.
4. Set enforcement to **Active**. Review all other applicable rulesets and
   branch protections; each must permit the App's direct push. With ordinary
   writers excluded, the queue enforces configured approvals/checks through Rego.
5. Test with an ordinary writer and the App on a disposable protected branch:
   the writer's merge/push should fail, and the App's authenticated push should
   succeed. Ruleset availability depends on the repository's GitHub plan.

Gauntlet does not create these rules or disable merge buttons itself. A mandatory
failing check alone does not reserve target updates, particularly when actors
can bypass it. With static credentials, reserve updates for their actual Git
transport identity; the API token account can differ from the pushing account.

App installation tokens authenticate API requests and Git pushes; comments appear
under the App's bot account. The Git commit's author remains the source author,
while the [configured committer](../reference/daemon.md) and optional
[commit signing](../reference/signing.md) are separate from push authentication.

## Verify

Push a disposable candidate and confirm `gauntlet/<target>` status transitions.
Without trial refs the status names the source SHA; with them it names the tested
commit. For PR intake, run `@gauntlet check` as an authorized user, request a merge,
and verify the bot's commit link and closed PR. Signed webhook registration is
separate; see [GitHub setup](github.md#polling-and-optional-webhooks).
