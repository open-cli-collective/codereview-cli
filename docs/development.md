# codereview-cli Development Guide

This is the repo-local guide for codereview-cli-specific facts. Shared Open CLI
Collective standards and automation remain canonical in their own repositories.

## Project Overview

codereview-cli is the future Open CLI Collective code-review CLI, intended to
ship the `cr` binary. The repository is currently a scaffold: it holds the Go
module, CI wiring, repo policy, and packaging placeholders before the command
surface lands.

The only Go code today is `internal/placeholder`, which keeps the tree compiling
until the real CLI is added.

## Quick Commands

```bash
make build   # compile the binary
make test    # go test ./...
make lint    # golangci-lint run
make tidy    # go mod tidy and verify go.mod is unchanged
make check   # tidy + lint + test + build
make clean   # remove build artifacts
```

`make snapshot` runs a local GoReleaser snapshot build without publishing.

## Repo-Local Shape

- Module: `github.com/open-cli-collective/codereview-cli`
- Intended binary: `cr`
- Main branch: `main`
- Local workflow files: `.github/workflows/ci.yml`,
  `.github/workflows/auto-release.yml`, and `.github/workflows/release.yml`
- Packaging identity: `packaging/identity.yml`
- Current distribution status: GitHub release archives only; package channels
  can be added when the real CLI needs them.

## Shared Standards

Use these sources for shared policy. Do not copy their mechanics into this file.

```md
Source of truth: https://github.com/open-cli-collective/cli-common/blob/main/docs/repo-layout.md
Local convenience copy, if present: `../cli-common/docs/repo-layout.md`

Source of truth: https://github.com/open-cli-collective/cli-common/blob/main/docs/ci.md
Local convenience copy, if present: `../cli-common/docs/ci.md`

Source of truth: https://github.com/open-cli-collective/cli-common/blob/main/docs/release.md
Local convenience copy, if present: `../cli-common/docs/release.md`

Source of truth: https://github.com/open-cli-collective/cli-common/blob/main/docs/distribution.md
Local convenience copy, if present: `../cli-common/docs/distribution.md`
```

## Shared Automation

Use this repository for shared action and reusable workflow implementations.

```md
Source of truth: https://github.com/open-cli-collective/.github
Local convenience copy, if present: `../.github`
```

## Repo Policy Summary

Keep `main` protected, require reviewed pull requests, keep the shared checks
green, use squash merge, and delete branches after merge. For the exact branch
protection and check contracts, use the shared standards above.
