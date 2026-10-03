# Hosting the daemon

A warm builder host is the simplest deployment: Gauntlet is a small executable
and the host supplies Git, check tools, runtimes, and credentials. Containerizing
the daemon changes packaging, not its execution or trust contracts.

## Warm builder host

1. Install Git 2.40+, the Gauntlet binary, and the tools used by your checks.
2. Create a dedicated service user and a persistent state directory.
3. Install operator config and provision [Git credentials](../guides/github-auth.md).
4. Run [validation and doctor](../guides/validating.md) as that service user.
5. Install the service using the [Linux](../runbooks/deploy-linux.md) or
   [macOS](../runbooks/deploy-macos-dev.md) runbook.
6. Run the [verification checklist](../runbooks/verify.md) against a disposable
   candidate before allowing production submissions.

For Azure, choose [a persistent VM](../runbooks/azure-vm.md) or
[an immutable VM with persistent data](../runbooks/azure-vm-immutable.md).

## Containerized daemon

Build the image with `make image` or use a published release image. Persist
state, mount operator config read-only, and keep the admin listener private:

```sh
docker run --rm --name gauntlet \
  -p 127.0.0.1:8080:8080 \
  -v gauntlet-state:/data \
  -v /etc/gauntlet/gauntlet.kdl:/etc/gauntlet/gauntlet.kdl:ro \
  ghcr.io/sgrankin/gauntlet:VERSION \
  -config /etc/gauntlet/gauntlet.kdl -state /data/state
```

Replace `VERSION` with a published tag and bind the dashboard to `0.0.0.0:8080`
inside the container. Put history under `/data` too; the image runs as UID 1000.
Provision the credentials required by the configured Git
transport separately; a state volume does not supply authentication.

A container executor needs access to its runtime and any operator-approved
mounts. Mounting a host Docker socket grants host authority; see
[container checks](../guides/containers.md). The daemon image does not contain
every project's toolchain.

## Lifecycle and upgrades

- Default shutdown drains admitted work and queued hooks. Set your service
  manager's stop timeout longer than the slowest expected validation/hook.
- A second signal forces cancellation. Restart recovers from remote history.
- Replace the binary or image, then restart. Use `gauntlet doctor` after changing
  configuration, credentials, or host tooling.
- Use [storage guidance](storage.md) for persistent state and backups,
  [security guidance](security.md) for ingress, and [release instructions](releases.md)
  for publishing a new version.
