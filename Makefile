.PHONY: all build test test-cover test-static-smoke test-pi-runtime pi-runtime-version check-pi-runtime-version lint fmt tidy deps check install snapshot package-render-check release clean

# Standard keyring tags: enable 1Password support, keep passage disabled.
GOFLAGS ?= -tags=keyring_nopassage
export GOFLAGS

all: check

build:
	go build -v ./...

test:
	go test -v ./...

test-cover:
	go test -coverprofile=coverage.out ./...

test-static-smoke:
	go test -v ./internal/... ./cmd/... -count=1

# Exact Pi version the installed-runtime gate requires. CI installs this
# version; test-pi-runtime fails, rather than skips, when `pi --version` on PATH
# reports anything else or Pi is missing.
PI_RUNTIME_VERSION ?= 1.0.0

# A blank pin would make the runtime tests skip and let npm install the latest
# Pi, so both targets below require exactly one version.
check-pi-runtime-version:
	@if [ "$(words $(PI_RUNTIME_VERSION))" != 1 ]; then \
		echo "PI_RUNTIME_VERSION must be exactly one Pi version" >&2; \
		exit 1; \
	fi

test-pi-runtime: check-pi-runtime-version
	CR_PI_RUNTIME_VERSION=$(strip $(PI_RUNTIME_VERSION)) go test -v -race -count=1 -run 'TestPiRPCRuntime|TestPiRPCReviewerExtensionLoadsInInstalledPi' ./internal/llmadapters

pi-runtime-version: check-pi-runtime-version
	@echo $(strip $(PI_RUNTIME_VERSION))

lint:
	golangci-lint run

fmt:
	go fmt ./...

tidy:
	go mod tidy
	@if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then \
		if [ -f go.sum ]; then \
			git diff --exit-code go.mod go.sum; \
		else \
			git diff --exit-code go.mod; \
		fi; \
	fi

deps:
	go mod download
	go mod verify

check: tidy fmt lint test build

install:
	go install ./cmd/cr

# goreleaser wrappers. `snapshot` builds locally without publishing (the same
# build CI's release.yml runs). `release` is the real publish and is intended for
# CI (via the reusable release workflow); it is guarded so a stray local run with
# GITHUB_TOKEN/GORELEASER_* in the environment can't accidentally publish — set
# CONFIRM_RELEASE=1 to override.
snapshot:
	goreleaser release --snapshot --clean --skip=publish
	scripts/verify-package-render.sh

package-render-check:
	scripts/verify-package-render.sh

release:
ifneq ($(CONFIRM_RELEASE),1)
	@echo "make release publishes a live release; this is CI-only." >&2
	@echo "Re-run with CONFIRM_RELEASE=1 if you really mean to publish locally." >&2
	@exit 1
endif
	goreleaser release --clean

clean:
	rm -rf bin/ dist/ coverage.out coverage.html
