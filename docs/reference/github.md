# Forge configuration

A `github` block enables commit statuses. Add `pull-requests` to import review
requests; otherwise it only reports Git-ref validation.

```kdl
github "acme/widgets" {
    token-env "GITHUB_TOKEN"
    pull-requests {
        bot "gauntlet"
        approvals 0
        require-resolved-conversations true
        require-check "lint"
    }
}
```

## Authentication

| Field | Default | Contract |
|---|---|---|
| GitHub argument | None | `owner/repo`; absence disables GitHub. |
| `token-env` | `"GITHUB_TOKEN"` | Static API token; must be populated at startup. Git transport uses ambient credentials. |
| `api-url` | `"https://api.github.com"` | Enterprise REST base, usually `https://HOST/api/v3`. |
| `auth "app"` | None | Exclusive with a static token; requires `app-id`, `installation-id`, and `private-key-file`. |

App mode uses refreshable installation tokens for both API and Git. The remote
must be credential-free HTTPS and match the configured repository and host.
The private key must have no group/other access (`0400` or `0600`). Rotation
requires restart; token refresh does not. Git's temporary askpass helper needs
an executable temporary directory. See [credentials and permissions](../guides/github-auth.md).

## Pull request intake

| Child of `pull-requests` | Default | Contract |
|---|---|---|
| `bot` | `"gauntlet"` | Mention prefix for commands. |
| `approvals` | `1` | 0–10 current-head writer approvals under shipped policy. |
| `require-check` | None | One or more host check/status names; all matching producers must pass. |
| `require-resolved-conversations` | `false` | All review threads, including outdated threads, must be resolved. |
| `poll-interval` | `"30s"`, or `"5m"` with webhook secret | Independent intake refresh interval; at least one second. |
| `webhook-secret-env` | None | Enables signed `/hooks/github` hints; requires dashboard and populated secret. |

Shipped Rego rules define requester authorization and readiness; custom policy
can extend or replace them. Approval count zero still retains trusted vetoes,
configured check gates, and conversation gates. Missing or incomplete forge
facts fail closed. See [policy](policy.md) and [GitHub commands/webhook setup](../guides/github.md).

## Trial refs

```kdl
github "acme/widgets" {
    trial-refs {
        prefix "refs/gauntlet/trials"
        retention "24h"
    }
}
```

Presence enables publication before verification. The immutable `<prefix>/<run-id>`
ref names the exact tested commit, and rollup status moves to that SHA. Conflict
runs report against the source because no trial commit exists. Publication
failure is an infrastructure error.

- `prefix` must start with `refs/`. Prefer a custom namespace; `refs/heads/`
  clutters branch lists and can trigger push workflows.
- `retention` defaults to 24 hours; explicit `0s` also means the default.
- Landed trial refs are deleted immediately. Failed trial refs expire by CAS;
  startup cleans crash-orphaned refs.

## Receipt notes

```kdl
github "acme/widgets" {
    receipt-notes {
        ref "refs/notes/gauntlet/receipts"
        max-bytes 65536
    }
}
```

Presence requires a [receipt node](receipts.md) in every covered check spec and
remotely confirmed publication on the exact tested SHA before target CAS.
`ref` must start with `refs/` and cannot be under `refs/heads/`. `max-bytes`
defaults to 65,536 and cannot exceed 1 MiB.

Publication is idempotent for identical bytes. Different bytes on the same SHA
fail closed; concurrent disjoint note updates retry with a bounded CAS loop.
Notes are append-only and may outlive unsuccessful landings. Verify the notes
transport with the daemon's real credentials before enabling it. See
[receipt consumers](../guides/receipts.md) and [storage](../operations/storage.md).

## Gerrit

```kdl
gerrit "https://review.example.com" {
    project "widgets"
    username-env "GERRIT_USERNAME"
    token-env "GERRIT_TOKEN"
    verification-requirement "Verified"
}
```

`project` is required; the shown credential variable names and verification
requirement are defaults. Credentials are stripped from candidate environments.
Git push authentication is separate. Gerrit and GitHub PR intake cannot both be
enabled in one daemon, and review targets must use squash landing.
See [Gerrit setup and submit semantics](../guides/gerrit.md).
