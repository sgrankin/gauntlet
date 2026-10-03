# Container executor

1. Choose Docker, Podman, or Apple's `container` CLI, and start its service.
   Set `runtime` explicitly (`"container"` is the default). Shared services
   require Docker or Podman. Runtime failures are infrastructure errors.
2. Build or pick an image containing whatever the check spec's commands
   need (a Go toolchain, `make`, …) — the executor doesn't provision
   anything beyond running the image.
3. Configure `executor "container"` with `image` and one `cache` entry per
   directory you want to persist (e.g. `GOCACHE`, `GOMODCACHE`) — these are
   named volumes that survive across runs, which is the point (DESIGN.md:
   persistent warm builder beats hermetic-ephemeral on speed).
4. Push a candidate; you should see a container start and stop per check
   (`container list` while a run is in flight), and a second run reusing
   the same image show faster build steps once caches are warm.
5. Need a check to reach the host docker daemon (the concrete driver: a
   repo whose test suite uses testcontainers) — add a `mount`:

   ```kdl
   executor "container" {
       runtime "docker"
       image "ghcr.io/acme/ci:latest"
       mount "/var/run/docker.sock" path="/var/run/docker.sock"
   }
   ```

   This is docker-out-of-docker: the check container talks to the *host's*
   docker daemon over the mounted socket and spawns sibling containers
   against it, rather than nesting a second daemon inside the check
   container. A few things to know before reaching for it:

   - **Testcontainers already works today, with zero config, under the
     `local` executor** — checks there are just host subprocesses with
     direct access to the host socket already. This `mount` knob is only
     for repos that want *both* the container executor's isolation *and*
     testcontainers.
   - **Mounting the docker socket hands every check full control of the
     host docker daemon.** Any ref anyone can push to `for/…` gets a check
     run against that mount, and the docker socket API is root-equivalent
     on most setups (a container run with `-v /:/host` is a sandbox
     escape). Only do this if every pusher is as trusted as an operator
     with shell on the builder host.
   - **`readonly` does not restrict the socket API.** `readonly` affects
     filesystem metadata (the check can't unlink/replace the socket file)
     — it has no effect on what the check can *say* to the daemon over
     that socket. Don't rely on it as a safety boundary here.
   - **Apple's `container` CLI has no host daemon socket to mount** — each
     container is its own lightweight VM with no shared daemon. On macOS,
     use `runtime "docker"` (Docker Desktop or colima) for this, or fall
     back to the `local` executor.
   - **Sibling-container paths are host paths, not check-container
     paths.** A path you hand to testcontainers for a bind mount (e.g.
     `Testcontainers.WithBindMount(...)`) is resolved by the *host* docker
     daemon against the *host* filesystem — a path inside the check
     container's own bind-mounted trial tree means nothing to it.
     Testcontainers' file-copy APIs (`CopyToContainer`/`WithFiles`, per
     your client library) sidestep this because they stream bytes over the
     API instead of naming a host path.

6. **docker-on-macOS footguns** (both found by live testing, both silent):
   - **The daemon's `-state` dir must live under a path the docker VM
     shares from the host.** colima shares only `$HOME` and `/tmp/colima`
     by default (Docker Desktop has its own file-sharing list). Trial
     trees are exported under `-state`, and `docker run -v` against an
     unshared host path does not error — it bind-mounts an *empty*
     directory, so every check fails with a confusing
     module/file-not-found red instead of an infra error. Either keep
     `-state` under `$HOME` or share it explicitly (e.g.
     `colima start --mount /path/to/state:w`). Gauntlet now detects this on
     a failed check (a quick post-mortem listing of the mount) and reports
     it as an infra error instead of a rejected red.
   - **The `osxkeychain` credential helper blocks headless pulls.** If an
     image isn't present locally, `docker run` pulls implicitly, the
     credential helper may pop a Keychain prompt — even for anonymous
     pulls of public images — and the check wedges until a human clicks.
     Pre-pull images used by checks (`docker pull` once, interactively),
     or drop `credsStore` from `~/.docker/config.json` on a headless
     builder.
