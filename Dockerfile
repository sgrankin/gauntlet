# Container image for gauntlet. See docs/deploy.md for the two deployment
# topologies this fits into.
#
# NOTE: the LOCAL executor running inside this image has no toolchains at
# all (no Go, no make, nothing) — a check spec that shells out to `go test`
# or similar will fail to even start. This image only suits deployments
# where either (a) the daemon is queue-only / GitHub-statuses-only and
# checks run elsewhere, or (b) the container executor is configured with a
# builder image of its own via a mounted container socket. The recommended
# topology — a warm builder VM with the toolchains already on the host — runs
# the plain binary directly on that host instead (docs/deploy.md §"warm
# builder VM"); it does not use this image at all.

FROM golang:1.26-alpine AS builder
ARG VERSION=devel
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/gauntlet ./cmd/gauntlet

FROM alpine:latest AS runtime

# Runtime deps, kept deliberately minimal:
#   git             >= 2.38, for `git merge-tree --write-tree` (trial
#                   merges) — alpine's packaged git is current, no pinning
#                   needed.
#   openssh-client  ssh remote auth (deploy key / agent-forwarded key).
#   ca-certificates https remotes, the GitHub REST API, Slack, and OTLP
#                   exporters all need a working TLS trust store.
#   tzdata          correct local-time rendering in logs and the dashboard.

# Fixed UID/GID (1000:1000) rather than whatever `adduser -S` picks: makes
# `chown -R 1000:1000 <host-state-dir>` a stable, documentable step for a
# bind-mounted /data (docs/deploy.md "container deployment").
RUN apk add --no-cache \
        git \
        openssh-client \
        ca-certificates \
        tzdata \
    && addgroup -g 1000 gauntlet \
    && adduser -S -u 1000 -G gauntlet -h /data gauntlet \
    && mkdir -p /data \
    && chown gauntlet:gauntlet /data

COPY --from=builder /out/gauntlet /usr/local/bin/gauntlet

# State (the bare repo clone(s), trial scratch dir, and history.db if
# configured) lives entirely under /data; see docs/deploy.md's "state dir
# layout" for what's disposable (trials/, history.db) vs. what to persist
# (repos/, and the config file itself if you keep it here rather than
# bind-mounting it separately).
VOLUME /data
USER gauntlet
WORKDIR /data

ENTRYPOINT ["gauntlet"]
CMD ["-config", "/data/gauntlet.kdl", "-state", "/data/state"]
