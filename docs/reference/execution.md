# Execution configuration

## Default and named profiles

The default executor is local. Use a kind argument for the default profile, or
an arbitrary name plus `kind` for a profile selected by repository checks:

```kdl
executor "container" {
    runtime "docker"
    image "ghcr.io/acme/ci:latest"
    cache "gomodcache" path="/go/pkg/mod"
}
executor "host" kind="local"
executor "integration" kind="container" {
    runtime "docker"
    image "ghcr.io/acme/ci:latest"
    mount "/var/run/docker.sock" path="/var/run/docker.sock"
    add-host "host.docker.internal" "host-gateway"
    env "TESTCONTAINERS_HOST_OVERRIDE" "host.docker.internal"
    memory "8g"
    cpus "4"
}
```

| Option | Default | Contract |
|---|---|---|
| Kind | `local` | `local` subprocesses or `container` checks. |
| `runtime` | `"container"` | Docker, Podman, or Apple's `container` CLI; container profiles only. |
| `image` | Required for container kind | Check image; project tools must already be installed. |
| `workdir` | `"/workspace"` | Read-write exported trial tree inside the container. |
| `cache NAME path=PATH` | None | Named persistent volume mounted at the given path. |
| `mount HOST path=PATH readonly=true` | None; writable unless set | Operator-approved bind mount. |
| `env NAME VALUE` | None | Fixed non-secret variables; `GAUNTLET_*` names are reserved. |
| `add-host NAME ADDRESS` | None | Container host aliases. |
| `memory`, `cpus` | Runtime defaults | Container resource ceilings. |

Profiles cannot be named `local`, `container`, or `default`. Repositories can
select an operator-defined profile but cannot change its mounts or authority.
An undefined profile fails before commands start. Prefer narrowly scoped profiles.

Every check receives `GAUNTLET_GIT_DIR`. Container checks mount it read-only at
`/gauntlet-git`; that path, the workdir, and `/gauntlet` results directory are
reserved against conflicting operator mounts. See the
[check environment](environment.md#check-environment-reference).

For socket mounts and testcontainers networking, follow the
[container guide](../guides/containers.md). Read-only socket mounts do not restrict
runtime commands; local checks and socket-enabled profiles require trusted code.

## Host capacity

`max-executions` caps bounded commands across targets, queue modes, and profiles,
including candidate checks and post-land hooks. Unset means unlimited; choose an
explicit production limit. Long-lived service instances have their own pool cap.
Waiting for capacity is recorded separately from command duration.

Demand can approach the sum of each target's `window × max-parallel`. The host
cap bounds this demand rather than assuming every repository remains serial.

## Shared services

```kdl
services {
    allow "container"
    max-instances 8
    runtime "docker"
}
```

- Only the `container` driver is implemented. Without `allow`, service requests
  are rejected rather than ignored.
- `max-instances` defaults to eight. It limits count, not resource usage.
- With a local default executor, the service runtime defaults to Docker and may
  be Docker or Podman.
- With a container default executor, that executor's runtime owns service
  networking; conflicting runtime settings are errors. Apple's `container` CLI
  cannot run shared services.
- Checks using services should use profiles matching the default executor's kind
  and runtime, so exported endpoints and networks remain reachable.

The [service reference](services.md) defines repository requests and variables.

## Export timestamps

```kdl
export {
    mtimes "history"
}
```

Without `export`, files receive extraction wall time. `mtimes "history"` stamps
tracked files using the committer time of the last change to each path in the
exact trial history, improving reuse in path/metadata-keyed caches.

- Renames count as changes at the new path; symlinks are stamped without following
  them. Directory mtimes are unchanged. Absent/export-ignored paths are not stamped.
- Future timestamps are retained. Merge products receive the merge timestamp;
  unchanged parent content keeps its deeper history timestamp.
- The history walk uses commit-date order. A newer discarded side of a merge may
  supply a timestamp rather than the surviving content's lineage.
- A bounded history walk runs per export. Failure is an infrastructure error;
  there is no silent wall-clock fallback.

This operator setting applies to check, image-build, and hook exports.
