.PHONY: all build test test-cover test-static-smoke lint fmt tidy deps check install snapshot package-render-check release clean

# Standard keyring tags: enable 1Password support, keep passage disabled.
export GOFLAGS := -tags=keyring_nopassage

all: check

build:
	go build -v ./...

test:
	go test -v ./...

test-cover:
	go test -coverprofile=coverage.out ./...

test-static-smoke:
	go test -v ./internal/... ./cmd/... -count=1

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
