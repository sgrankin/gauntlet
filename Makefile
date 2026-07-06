# Gauntlet build/test/package targets. GNU make (the one on macOS and
# Linux); no attempt at BSD-make portability. See docs/deploy.md for how
# these fit into the two deployment topologies.

BINARY  := gauntlet
IMAGE   := gauntlet

# Version derivation: `git describe --always --dirty` if a git checkout is
# present and describable, else "devel". This is the ONLY git usage in this
# Makefile, and it is read-only — never run a mutating git command here.
VERSION := $(shell git describe --always --dirty 2>/dev/null || echo devel)

LDFLAGS := -X main.version=$(VERSION)

# One of docker/podman/container (Apple's CLI), whichever is found first.
# Override on the command line (`make image RUNTIME=podman`) to force one.
RUNTIME := $(shell command -v docker 2>/dev/null || command -v podman 2>/dev/null || command -v container 2>/dev/null)

.PHONY: build test image clean release-snapshot release

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/gauntlet

test:
	go test -race -count=1 ./...

image:
	@if [ -z "$(RUNTIME)" ]; then \
		echo "make image: no container runtime found on PATH (docker, podman, or container)" >&2; \
		exit 1; \
	fi
	$(RUNTIME) build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -f Dockerfile .

clean:
	rm -f $(BINARY)

# Local dry-run of the tagged-release pipeline (.goreleaser.yaml): builds
# every target archive and the ghcr images without publishing anything or
# needing a real tag/git history. goreleaser is pinned as a go.mod tool
# dependency (`tool` directive), so `go tool` runs the exact version the
# module records — same binary CI uses. See docs/deploy.md "Releases".
release-snapshot:
	go tool goreleaser release --snapshot --clean --skip=publish,docker

# Cut a real tagged release: `make release VERSION=v0.1.0`. Operator-run
# only — never invoked by CI or any other automation. Validates VERSION,
# refuses if the working copy has uncommitted changes or has diverged from
# origin/main, then tags and pushes; pushing a `v*` tag is what triggers
# .github/workflows/release.yml (goreleaser does the rest).
release:
	@if [ "$(origin VERSION)" != "command line" ]; then \
		echo "make release: VERSION must be given explicitly on the command line, e.g. make release VERSION=v0.1.0 (the top-of-file VERSION default is for build/image, not this)" >&2; \
		exit 1; \
	fi
	@case "$(VERSION)" in \
		v[0-9]*) ;; \
		*) echo "make release: VERSION must match ^v[0-9] (got '$(VERSION)')" >&2; exit 1 ;; \
	esac
	@git diff --quiet || { echo "make release: working copy has uncommitted changes" >&2; exit 1; }
	@git fetch origin
	@[ "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/main)" ] || { \
		echo "make release: HEAD does not match origin/main — push/pull first" >&2; \
		exit 1; \
	}
	git tag $(VERSION)
	git push origin $(VERSION)
