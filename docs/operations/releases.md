# Releases

## Releases

Both [hosting options](hosting.md) can be fed from tagged releases instead of a local
`make build`/`make image`. Cutting one is one command:

```sh
make release VERSION=v1.4.0
```

This validates `VERSION` (must match `v[0-9]...`), refuses if the working
copy has uncommitted changes or has diverged from `origin/main`, then tags
and pushes — pushing a `v*` tag is what triggers
`.github/workflows/release.yml`, which drives `goreleaser`
(`.goreleaser.yaml`) to publish, on the GitHub release page, raw
`gauntlet_linux_amd64` / `gauntlet_linux_arm64` / `gauntlet_darwin_arm64`
binaries (no archive/extraction step — just the executable) plus a
`checksums.txt`, and, to `ghcr.io/sgrankin/gauntlet`, a multi-arch
(`linux/amd64`+`linux/arm64`) image tagged both `<version>` and `latest`.
Every push to `main` and every pull request separately runs
`.github/workflows/ci.yml` (`go mod tidy` drift check, `go build`, `go vet`,
`go test -race`) — the release workflow only runs on a tag push, and does not
re-run the test suite itself. `make release-snapshot` (see the Makefile) is
the local dry-run of the goreleaser pipeline, skipping publish and docker.

**Asset naming, read carefully before scripting against it:** the binary
name_template is `{{ .ProjectName }}_{{ .Os }}_{{ .Arch }}` — it carries
**no version**. What distinguishes one release's assets from another's is
which GitHub release (tag) they're attached to, not the filename itself; the
version only ever appears in the release tag / download URL path segment
(`.../releases/download/v1.4.0/gauntlet_linux_amd64`), never in the asset
name. Getting this backwards (expecting a versioned filename) produces a
404, not a wrong-version download.

- **Topology (a)** (warm builder VM): `curl -fsSL -o /usr/local/bin/gauntlet`
  the release binary for your arch from the GitHub release page and `chmod
  +x` it, instead of `scp`-ing a locally built one; everything else in that
  topology's setup/upgrade steps is unchanged. See
  [azure-vm.md](../runbooks/azure-vm.md) for the exact fetch commands.
- **Topology (b)** (container): `docker pull ghcr.io/sgrankin/gauntlet:<version>`
  (or `:latest`) instead of `make image`; the `docker run` invocation and
  [state layout](storage.md) are identical either way. Image tags carry **no
  `v` prefix** — the release tag `v1.4.0` publishes `:1.4.0` (goreleaser's
  `{{ .Version }}` strips the `v`); pulling `:v1.4.0` is a manifest-unknown
  error, the docker-tag cousin of the asset-naming 404 above.
- **Why the release image isn't built from the top-level [`Dockerfile`](https://github.com/sgrankin/gauntlet/blob/main/Dockerfile):**
  goreleaser's docker builder (`dockers_v2`, one buildx multi-platform
  build) copies the prebuilt per-platform binaries into a throwaway context
  rather than running a multi-stage build, so releases use a separate,
  runtime-stage-only `Dockerfile.release` that mirrors this Dockerfile's
  runtime contract (packages, fixed UID, `/data` volume, entrypoint)
  byte-for-byte; the original `Dockerfile` stays the one used for
  from-source builds (`make image`). A plain `ko` build was rejected instead,
  since ko's default base images ship no `git`, and the daemon shells out to
  `git` at runtime for every trial merge.
- `gauntlet -version` prints the same version either way — ldflags-stamped
  from the pushed tag by goreleaser, or from `git describe` by `make build`.
