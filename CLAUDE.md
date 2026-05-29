# CLAUDE.md

This file provides guidance for AI agents working with the codereview-cli codebase.

## Project Overview

codereview-cli is the future Open CLI Collective code-review CLI (intended binary
`cr`), written in Go. The repository is currently a **scaffold**: it holds the
module, CI, repo policy, and packaging conventions before the command surface
lands. The only Go code today is `internal/placeholder`, which exists so the tree
compiles and the checks stay green until the real CLI is added.

This repo follows the family-wide cli-common standards:
- [`docs/repo-layout.md`](https://github.com/open-cli-collective/cli-common/blob/main/docs/repo-layout.md) — layout, Go version (go.mod is the single source), Makefile contract, `.golangci.yml` floor linter set.
- [`docs/ci.md`](https://github.com/open-cli-collective/cli-common/blob/main/docs/ci.md) — CI runs `build`/`test`/`lint` as separate jobs plus a PR-only `pr-title` check, all via the shared composite actions in `open-cli-collective/.github`.

## Quick Commands

```bash
make build   # compile the binary/binaries
make test    # go test ./...
make lint    # golangci-lint run (against ./.golangci.yml)
make tidy    # go mod tidy + verify go.mod is unchanged
make check   # tidy + lint + test + build — the local pre-push gate
make clean   # remove build artifacts
```

`make check` mirrors what CI gates on, so a green local `check` predicts a green
CI run.

## CI

`.github/workflows/ci.yml` calls the shared composite actions at `@v1`:

- `build-platform` — `make build` across the ubuntu/macos/windows matrix (not a
  required check); `build` — the required aggregate over it.
- `test` — `make test` via `go-test@v1`.
- `lint` — `golangci-lint` via `go-lint@v1` (the golangci version is pinned inside
  the composite, so a family-wide bump is one edit in `.github`).
- `pr-title` — asserts the PR title is a conventional commit (squash-merge makes
  the PR title the release-gating commit); `pull_request` only.

The Go toolchain version comes from `go.mod` (`go-version-file: go.mod`) — never
hardcode a second value in the workflow.

## Repo policy

Branch protection on `main`: required PR with 1 approval, required status checks
`build`/`test`/`lint`/`pr-title`, signed commits, linear history, squash-merge
only, delete-branch-on-merge. Release machinery (`version.txt`, `.goreleaser.yml`,
`packaging/identity.yml`, the reusable release callers, and the `identity-check`
required check) arrives in a follow-up.
