.PHONY: all build test test-cover lint fmt tidy check install snapshot release clean

all: check

build:
	go build -v ./...

test:
	go test -v ./...

test-cover:
	go test -coverprofile=coverage.out ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

tidy:
	go mod tidy
	@if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then \
		if [ -f go.sum ]; then \
			git diff --exit-code go.mod go.sum; \
		else \
			git diff --exit-code go.mod; \
		fi; \
	fi

check: tidy lint test build

install:
	go install ./cmd/cr

# goreleaser wrappers. `snapshot` builds locally without publishing (the same
# build CI's release.yml runs); `release` is the real publish (CI uses it via the
# reusable workflow — run locally only with the right env/tokens).
snapshot:
	goreleaser release --snapshot --clean

release:
	goreleaser release --clean

clean:
	rm -rf bin/ dist/ coverage.out coverage.html
