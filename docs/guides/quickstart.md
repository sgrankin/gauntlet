# Quickstart

Use a disposable repository for the first run. Gauntlet needs Git 2.40 or newer
and credentials that can fetch candidates and push the target branch. Build
with the Go version declared in `go.mod`:

```sh
git clone https://github.com/sgrankin/gauntlet.git
cd gauntlet
make build
```

## Define validation in your repository

Commit `.gauntlet.kdl` on the target branch:

```kdl
check "test" {
    command "go" "test" "./..."
}
```

Commands run in the exported trial tree. The local executor uses the daemon's
host tools; select a [container executor](containers.md) for a prepared image.

## Configure the daemon

Create `gauntlet.kdl` outside candidate work directories:

```kdl
remote "git@github.com:acme/widgets.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
history "/var/lib/gauntlet/history.db"
dashboard "localhost:8080"
```

Replace the remote and identity. The service user must own its state and
history directories. Validate, probe the host, then start:

```sh
./gauntlet validate -config gauntlet.kdl
./gauntlet doctor -config gauntlet.kdl
./gauntlet -config gauntlet.kdl -state /var/lib/gauntlet
```

Open `http://localhost:8080`. Keep this listener private: its operator API
has no built-in authentication. See [security](../operations/security.md).

## Submit a change

From your project checkout:

```sh
git push origin HEAD:refs/heads/for/main/alice/first-change
```

Gauntlet squashes the submission onto the current target, runs the trial's
check spec, and publishes the exact tested commit if it passes. A successful
landing removes the candidate ref; a failure parks that revision.

Next: [retry or cancel a submission](landing.md), [connect GitHub](github.md),
or [install the daemon on a host](../operations/hosting.md).
