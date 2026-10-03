# Security and access

## Host requirements

Use Git 2.40 or newer for `merge-tree --write-tree --merge-base`. The daemon
checks the version at startup. `gauntlet doctor` probes tools, credentials,
remote access, and configured runtime profiles before service installation.

## Dashboard, API, and MCP

These share one listener and have no built-in authentication. They expose
operator actions including cancellation, pause/resume, drain, and configured
emergency merges. Treat access as administrative authority.

- Bind to localhost or a trusted interface.
- Use authenticated reverse-proxy or tailnet ingress for remote access.
- Set `dashboard { url ... }` to the public base URL for outbound links.
- Expose only `/hooks/github` publicly when using signed webhooks; keep the
  dashboard, API, and `/mcp` behind access control.

The standalone API uses one trusted ingress principal for policy. A supplied
`Actor` is an audit label, not authentication. Embedded handlers may establish
individual identities with `WithPrincipalResolver`; arbitrary HTTP headers are
not trusted. See [policy identity](../reference/policy.md#identity-and-diagnostics).

## Candidate execution

Local checks run as the daemon's OS user. Configured operator secrets are
removed from command environments, but this is not filesystem or process
isolation. Socket mounts give container checks the host runtime's authority;
read-only socket mounts do not restrict the socket API. Only grant these
profiles to trusted candidate code.

Keep credentials and signing keys outside the bare repository and trial trees.
Use [GitHub authentication](../guides/github-auth.md) and
[signing](../reference/signing.md) for the relevant permissions and key handling.
