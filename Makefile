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

.PHONY: build test image clean release-snapshot

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
