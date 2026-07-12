# codereview-cli Development Guide

This is the repo-local guide for codereview-cli-specific facts. Shared Open CLI
Collective standards and automation remain canonical in their own repositories.

## Project Overview

codereview-cli is the Open CLI Collective code-review CLI and ships the `cr`
binary. It provides configuration and credential commands, trusted-agent
inspection, dry-run and live pull-request review orchestration, inline thread
response handling through `cr respond`, named LLM session management, and local
data lifecycle commands.

The current Go code is a Cobra command tree in `internal/cmd/*` with a thin
`cmd/cr` entrypoint, shared exit-code mapping in `internal/cmd/exitcode`, and
version plumbing in `internal/version`. Production review and response runtime
assembly lives in `internal/app`; `internal/appruntime` contains only small
command-independent helpers such as retention conversion and invocation-root
resolution. Command packages keep CLI composition, rendering, config-path
selection, and error mapping. Review orchestration is split across
`internal/pipeline`, `internal/reviewrun`,
`internal/threadrespond`, `internal/reviewplan`, `internal/outbox`,
`internal/gate`, and `internal/gateio`. Command packages construct typed
runtime requests, while `internal/app` owns the reusable provider, ledger,
adapter, pipeline, live-run, and response-run assembly.

Architecture guardrails for LLM execution, model resolution, Git provider
writes, command and review harness boundaries, inline thread lifecycle, and
retention live in
[`docs/architecture.md`](architecture.md).

The temporary architecture refactor workstream context for issue #420 lives in
[`docs/architecture-refactor-workstream.md`](architecture-refactor-workstream.md).
While that workstream is active, extend the named acceptance harness tests when
a change could regress review composition or dependency boundaries rather than
cloning broad review assertions. Retire or promote the workstream context
document when issue #420 completes. That temporary doc tracks #420-specific
sequencing and the current child-issue harness inventory under "Harness And
Guardrails".

Within `internal/pipeline`, the public entry points are `DryRun`, `Live`, and
`SelectionOnly`. `DryRun` and `Live` execute the full review pipeline, while
`SelectionOnly` runs just the selection phase with caller-owned artifact paths
and no ledger or posting side effects so benchmark tooling can reuse the real
selector implementation.

Review workbench preparation is location-independent. Runtime composition
constructs the authenticated Git command adapter from the repository/read
credential and injects it into the pipeline; workbench code derives remotes
from the validated PR identity and must not resolve or inspect the invocation
checkout.

Structured LLM calls in the review pipeline are durable per-task units. See
`docs/llm-task-artifacts.md` for the artifact schema, status taxonomy, and
resume invariants.

The target checkout-native review design for the "avoid context stuffing" work
is documented in `docs/checkout-native-review-contract.md`. Treat that file as
the source of truth for runtime order, reviewer-facing dossier boundaries,
workbench ownership, and artifact-digest resume rules while the follow-on
pipeline issues land.

## Progress Logging

Non-review CLI progress now flows through one reusable component in
`internal/progress`. The contract for this slice is:

- progress writes to stderr only
- JSON and text result payloads stay on stdout
- `--quiet` suppresses progress only
- lines are single-line structured records:
  `cr progress event=<start|finish|error> command="..." op="..." target="..." ...`

Current command coverage is `benchmark run`, `benchmark select`,
`benchmark compare`, `data prune`, `data purge`, `config clear`, `me`, and
`sessions delete`.

Command packages own the `command=` label and stderr sink. Lower-level reuse is
intentionally narrow: `internal/datalifecycle` accepts a tiny start/end
progress interface rather than importing Cobra or root options.

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
- Support scripts are documented in `scripts/README.md`.
- Local workflow files: `.github/workflows/ci.yml`,
  `.github/workflows/auto-release.yml`, and `.github/workflows/release.yml`
- Packaging identity: `packaging/identity.yml`
- Current distribution status: GitHub release archives plus standard package
  channels declared in `packaging/identity.yml`.
- Application package layering follows the active codereview implementation:
  command glue in `internal/cmd/*`, production review/response composition in
  `internal/app`, small runtime helpers in `internal/appruntime`, presentation in `internal/view`,
  state/config adapters in `internal/config`, `internal/ledger`, and
  `internal/statepaths`, provider/LLM adapters in their owning packages, and
  review posting/gating in `internal/outbox`, `internal/gate`, and
  `internal/gateio`, and response-only
  inline discussion handling in
  `internal/threadrespond`.

## Interactive Init Notes

`cr init` interactive mode keeps all writes draft-local until the user commits
staged changes. Reviewer setup may collect PAT or GitHub App reviewer secrets
inside the reviewer-entity flow, but those values are only written to the
credential store during **Commit staged changes and exit**. When the selected
LLM auth mode is `api_key`, the wizard saves the non-secret profile shape and
prints a follow-up `cr set-credential` command instead of collecting the API key
inline.

Interactive secret fields stay blank even when a credential key already exists.
The helper text carries the state instead: missing secrets prompt for entry,
known existing secrets say blank preserves the current value and a new value
replaces it, and staged values distinguish new secrets from replacement of an
exact known credential ref. Init only treats an inline secret as overwrite
intent when the exact credential ref is known to exist; otherwise the normal
conflict check still protects against accidental name collisions.

Interactive `git.host` edits now route through the repository-route stage when
the target profile already participates in `repository_profiles` routing. The
wizard shows the affected routes, lets the user reconcile or remove them, and
rejects preserved routes whose host no longer matches the selected profile.

Default repo builds now compile 1Password keyring backend support in by keeping
`keyring_nopassage` but dropping `keyring_no1password` from shipped build tags.
CI still runs an explicit `keyring_no1password` opt-out test path plus a
`CGO_ENABLED=0` smoke test so unsupported build modes fail intentionally rather
than drifting unnoticed.

Use the `keyring_no1password` build tag for distributions that must exclude
1Password integration entirely, such as FIPS-constrained, air-gapped, or
otherwise compliance-scoped builds. In that mode the named secrets-management
workflow hides all 1Password backends and validation rejects 1Password-backed
config up front instead of failing later at runtime.

## Release Secrets

`auto-release.yml` passes `RELEASE_TAG_TOKEN` to the shared auto-release
workflow as its `tag-token`. That credential must be separate from
`GITHUB_TOKEN` so tag pushes retrigger `release.yml`, and separate from
package-channel credentials. The preferred long-term shape is the GitHub App
installation-token path described in the shared release standard; until that is
wired through, use a narrowly scoped `RELEASE_TAG_TOKEN`.

`TAP_GITHUB_TOKEN` is only for Homebrew tap pushes from the release workflow.
Do not reuse it for auto-release tag pushes.

Package-channel releases also use `CHOCOLATEY_API_KEY`,
`WINGET_GITHUB_TOKEN`, and `LINUX_PACKAGES_DISPATCH_TOKEN` according to the
shared distribution secret table.

## Package Channel Release Mechanics

`release.yml` delegates package publishing to the shared reusable release
workflow. Repo-local files declare package identity and native metadata; the
shared workflow owns publish mechanics.

- Homebrew: GoReleaser renders `dist/homebrew/Casks/codereview-cli.rb` with
  `skip_upload: true`; the shared Homebrew job pushes that generated cask to
  `open-cli-collective/homebrew-tap` with `TAP_GITHUB_TOKEN`.
- Chocolatey: the shared Chocolatey job rewrites
  `packaging/chocolatey/codereview-cli.nuspec` from version `0.0.0` to the release version
  and replaces `CHECKSUM_AMD64_PLACEHOLDER` /
  `CHECKSUM_ARM64_PLACEHOLDER` in `chocolateyInstall.ps1` before packing and
  pushing.
- winget: the shared winget job resolves the release asset URLs/checksums and
  submits `OpenCLICollective.codereview-cli` with `WINGET_GITHUB_TOKEN`;
  because this is a first-time package id, `packaging/identity.yml` keeps
  `packages.winget.bootstrap: true` until the package exists upstream.
- Linux packages: GoReleaser renders `.deb` and `.rpm` artifacts named `cr`;
  the shared workflow dispatches `package-release` to
  `open-cli-collective/linux-packages` with `LINUX_PACKAGES_DISPATCH_TOKEN`.

`make snapshot` runs a local GoReleaser snapshot and then
`scripts/verify-package-render.sh`, which asserts the rendered cask, deb/rpm
artifacts, native package templates, and release secret wiring match the
declared package IDs.

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
