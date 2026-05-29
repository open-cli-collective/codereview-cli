.PHONY: all build test lint tidy check clean

all: check

build:
	go build -v ./...

test:
	go test -v ./...

lint:
	golangci-lint run

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

clean:
	rm -rf bin/ dist/ coverage.out coverage.html
