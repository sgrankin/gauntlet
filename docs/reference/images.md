# Candidate-built images

A container check normally runs in its profile's static image. When the
toolchain image evolves *with* the code, declare a named image and let the
candidate build it in the same merge transaction — no operator pre-publish
step, no mutable tag meaning different bytes to two checks:

```kdl
image "go-ci" {
    command "./ci/images/go-ci/build"
}

check "unit" {
    command "./ci/unit"
    image "go-ci"
}
check "lint" {
    command "./ci/lint"
    image "go-ci"
}
```

The build command is opaque repo code, run against the trial tree with the
normal `GAUNTLET_*` variables plus `GAUNTLET_IMAGE_RESULT_FILE` (in place
of `GAUNTLET_RESULT_FILE` — builds have no skipped verdict). It must exit
0 **and** write exactly one *immutable* reference there — a local image ID
or a digest-pinned registry reference; a mutable tag is rejected:

```sh
#!/bin/sh
set -eu
docker buildx build --load --iidfile "$GAUNTLET_IMAGE_RESULT_FILE" \
    -f ci/images/go-ci/Dockerfile ci/images/go-ci
```

The build is scheduled as a node named `image:go-ci` in the same
dependency graph as your checks (that name prefix is reserved): built at
most once per run, an implicit `after` prerequisite of every check naming
it, taking one `max-parallel`/`max-executions` slot like any command.
Every consumer then runs by the captured identity — recorded in history
for both the build and its consumers, so a run always says exactly which
bytes ran. A failed build (non-zero exit, or an empty/mutable/multi-line
result) is ONE root cause: the build's own red row, with every consumer
`blocked` on it — never N unrelated consumer failures. Validation is
syntactic: gauntlet checks the reference's *shape*, not that the runtime
can actually see it — a build that writes a well-formed ID for an image
it never loaded (e.g. `buildx` without `--load`) surfaces as the first
consumer's infrastructure error instead.

Boundaries: consumers need a container-kind executor profile (gated at
run start); a build runs on an ordinary operator profile (typically one
with the docker socket) and can never itself depend on a candidate-built
image — multi-image relationships belong in your Dockerfiles or build
program. Capturing an ID makes this run exact; it does not make a
floating `FROM` reproducible forever — pin base images by digest when
that matters. BuildKit owns cross-run layer caching; a pruned image just
rebuilds. Gauntlet never learns Dockerfiles, contexts, or cache keys.

One combination to avoid: `executor "<name>"` together with `needs`.
Shared-service endpoints are wired for the daemon's *default* executor
(its kind picks published-port vs shared-network mode; its runtime owns
the network), so a `needs` check on a profile of a different kind or
runtime can receive endpoints it cannot reach. Keep service-dependent
checks on the default executor unless your operator confirms the profile
matches it.
