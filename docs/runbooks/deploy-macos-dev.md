# Runbook: run gauntlet locally on macOS (dev loop)

**What you get:** the `gauntlet` binary running in the foreground (or under
launchd) on your Mac, checks executed in containers via colima, dashboard
on localhost. This is the fast local dev-loop shape, not a production
deploy — see [deploy-linux.md](deploy-linux.md) for that.

**Prerequisites**

- macOS with [colima](https://github.com/abiquo/colima) installed
  (`brew install colima docker`) — Docker Desktop works too, but the mount
  footgun below is colima-specific; if using Docker Desktop, use its own
  file-sharing settings instead of step 2.
- `git` ≥ 2.38 (`git --version` — recent macOS ships new enough git via
  Xcode CLT or Homebrew).
- Go toolchain, if building from source (`go build -o gauntlet ./cmd/gauntlet`).

**Two footguns that cost real debugging time — read before starting:**

1. **colima only shares `$HOME` and `/tmp/colima` with the VM by default.**
   If `-state` lives outside `$HOME`, `docker run -v` against it does not
   error — it silently bind-mounts an **empty** directory, and every check
   fails with a confusing "does not contain main module" / file-not-found
   red instead of an infra error. Either keep `-state` under `$HOME`, or add
   `--mount <STATE_PARENT_DIR>:w` to every `colima start` (see step 2).
2. **`osxkeychain` credential helper blocks headless pulls with a GUI
   prompt** — even for anonymous pulls of public images. If an image isn't
   already local, `docker run`'s implicit pull can pop a Keychain prompt and
   wedge the check until a human clicks it. Pre-pull every image your check
   spec uses (step 3), or drop `credsStore` from `~/.docker/config.json`.

---

## Phase 1 — Start colima

1. Pick a state directory under `$HOME` (simplest — skip the `--mount` flag
   entirely) or elsewhere (then you must pass `--mount`):

   ```sh
   # simple case: state under $HOME, no extra sharing needed
   colima start --vz-rosetta
   ```

   ```sh
   # state OUTSIDE $HOME (e.g. /opt/gauntlet-state) — must share it explicitly.
   # Re-run this EVERY time you recreate the colima VM (colima delete + start),
   # not just the first time — the mount doesn't persist across a recreate.
   colima start --vz-rosetta --mount /opt/gauntlet-state:w
   ```

   `--vz-rosetta` is only needed if any check/service image is amd64-only
   (e.g. SQL Server has no arm64 image) — omit it if everything you run is
   multi-arch or arm64-native.

2. **VERIFY**

   ```sh
   colima status
   # expect: colima is running
   docker version
   # expect: both Client and Server sections print, no connection error
   ```

## Phase 2 — Lay out state

```sh
mkdir -p ~/.cache/gauntlet
```

(Or your chosen `--mount`-shared path from step 1. `gauntlet`'s `-state`
default is `os.UserCacheDir()/gauntlet`, i.e. `~/Library/Caches/gauntlet` —
fine as-is, since that's under `$HOME`.)

## Phase 3 — Pre-pull images

Pre-pull every image your check spec's `executor "container"` or `service`
blocks reference, **before** the first run — this sidesteps the
`osxkeychain` prompt footgun above entirely:

```sh
docker pull <CHECK_BUILDER_IMAGE>
docker pull <SERVICE_IMAGE>   # if using a services block, e.g. mssql
```

**VERIFY**

```sh
docker images | grep <CHECK_BUILDER_IMAGE>
# expect: the image listed with a real SIZE/CREATED, not empty
```

## Phase 4 — Configure

Write `gauntlet.kdl` (anywhere, e.g. `~/gauntlet.kdl`):

```kdl
remote "<REMOTE_URL>"          // e.g. git@github.com:<OWNER>/<REPO>.git
poll-interval "5s"             // shorter than production — faster dev feedback
check-spec ".gauntlet.kdl"

committer {
    name "Gauntlet Dev"
    email "gauntlet-dev@<YOUR_DOMAIN>"
}

target "main" branch="main"

dashboard "localhost:8080"

executor "container" {
    runtime "docker"           // colima presents itself to the docker CLI as the docker runtime
    image "<CHECK_BUILDER_IMAGE>"
    cache "gocache"    path="/root/.cache/go-build"
    cache "gomodcache" path="/go/pkg/mod"
}
```

Drop the `executor` block entirely to run checks as local subprocesses on
your Mac directly instead (no colima needed at all, if that's all you're
testing).

**VERIFY** (dry parse)

```sh
timeout 2 gauntlet -config ~/gauntlet.kdl -state ~/.cache/gauntlet; echo "exit: $?"
# expect: no config-parse error before the timeout kills it (exit 124 is fine)
```

For a fuller host preflight beyond config parsing (git version, `-state`,
GitHub auth, remote reachability, executor runtimes, dashboard port), run
`gauntlet doctor -config ~/gauntlet.kdl -state ~/.cache/gauntlet` instead —
see [deploy.md](../deploy.md#preflight-gauntlet-doctor).

## Phase 5 — Run it

**Foreground** (simplest for a dev loop — Ctrl-C stops it cleanly, `SIGINT`
is handled):

```sh
gauntlet -config ~/gauntlet.kdl -state ~/.cache/gauntlet
```

**Or under launchd**, if you want it to survive terminal closes — write
`~/Library/LaunchAgents/com.gauntlet.dev.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.gauntlet.dev</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/gauntlet</string>
        <string>-config</string><string><YOUR_HOME>/gauntlet.kdl</string>
        <string>-state</string><string><YOUR_HOME>/.cache/gauntlet</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>StandardOutPath</key><string><YOUR_HOME>/Library/Logs/gauntlet.log</string>
    <key>StandardErrorPath</key><string><YOUR_HOME>/Library/Logs/gauntlet.log</string>
</dict>
</plist>
```

```sh
launchctl load ~/Library/LaunchAgents/com.gauntlet.dev.plist
```

**VERIFY**

```sh
curl -s http://localhost:8080/api/v1/status | jq .
# expect: JSON with a "targets" array, HTTP 200 (503 "no snapshot yet" briefly right after start is normal)
```

## Operations

**Two daemons can't share one `-state` dir** — a second `gauntlet` process
against the same path refuses to start (flock). If you're iterating with
both a foreground run and a launchd copy, stop one first.

**Restart after a colima recreate:** re-run the exact `colima start` command
from phase 1 (with `--mount` if you used it) before starting gauntlet again
— the mount does not persist across `colima delete`.

**Logs:** foreground prints to your terminal; launchd writes to
`~/Library/Logs/gauntlet.log`. Full per-check logs:
`zstd -d ~/.cache/gauntlet/logs/<runID>/<check>.log.zst`.

**Land a candidate for testing:**

```sh
gauntlet land -target main -topic my-feature
# or: git push origin HEAD:refs/heads/for/main/$USER/my-feature
```

See [verify.md](verify.md) for the full acceptance checklist, and
[deploy-linux.md](deploy-linux.md) for the production shape this dev loop
mirrors.
