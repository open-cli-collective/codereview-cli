# codereview-cli Development Guide

This is the repo-local guide for codereview-cli-specific facts. Shared Open CLI
Collective standards and automation remain canonical in their own repositories.

## Project Overview

codereview-cli is the Open CLI Collective code-review CLI and ships the `cr`
binary. It provides configuration and credential commands, trusted-agent
inspection, dry-run and live pull-request review orchestration, named LLM
session management, and local data lifecycle commands.

The current Go code is a Cobra command tree in `internal/cmd/*` with a thin
`cmd/cr` entrypoint, shared exit-code mapping in `internal/cmd/exitcode`, and
version plumbing in `internal/version`. Review orchestration is split across
`internal/pipeline`, `internal/reviewrun`, `internal/reviewplan`,
`internal/outbox`, `internal/gate`, and `internal/gateio`.

## Quick Commands

```bash
make build   # compile the binary
make test    # go test ./...
make lint    # golangci-lint run
make tidy    # go mod tidy and verify go.mod is unchanged
make deps    # download and verify Go modules
make check   # tidy + fmt + lint + test + build
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
- Current distribution status: GitHub release archives plus standard package
  channels declared in `packaging/identity.yml`.
- Application package layering follows the active codereview implementation:
  command glue in `internal/cmd/*`, presentation in `internal/view`,
  state/config adapters in `internal/config`, `internal/ledger`, and
  `internal/statepaths`, provider/LLM adapters in their owning packages, and
  review posting/gating in `internal/outbox`, `internal/gate`, and
  `internal/gateio`.

## Release Secrets

`auto-release.yml` passes `RELEASE_TAG_TOKEN` to the shared auto-release
workflow as its `tag-token`. That credential must be separate from
`GITHUB_TOKEN` so tag pushes retrigger `release.yml`, and separate from
package-channel credentials. The preferred long-term shape is the GitHub App
installation-token path described in the shared release standard; until that is
wired through, use a narrowly scoped `RELEASE_TAG_TOKEN`.

`HOMEBREW_TAP_TOKEN` is only for Homebrew tap pushes from the release workflow.
Do not reuse it for auto-release tag pushes.

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

Use `open-cli-collective/.github` for shared action and reusable workflow
implementations.

```md
Source of truth: https://github.com/open-cli-collective/.github
Local convenience copy, if present: `../.github`
```

## Repo Policy Summary

Keep `main` protected, require reviewed pull requests, keep the shared checks
green, use squash merge, and delete branches after merge. For the exact branch
protection and check contracts, use the shared standards above.
