# codereview-cli

Repository scaffold for the future Open CLI Collective code review CLI.

The actual review program is still pending. This repo exists to hold the
module, CI, repo policy, and packaging conventions before the command
surface lands.

## Current shape

- Go module: `github.com/open-cli-collective/codereview-cli`
- Intended CLI name: `cr`
- Branch protection target: `main`

## Repo policy

Use GitHub branch protection on `main` with:

- required pull request reviews
- at least one approval
- stale review dismissal
- required status checks for `build`, `test`, `lint`, `pr-title`, and `identity-check`
- no direct pushes
- squash merge only

## Development

```bash
make tidy
make lint
make test
make build
make snapshot   # local goreleaser build (no publish)
```

The tree currently holds a scaffold `cr` binary (`cmd/cr`) that reports its
build version, plus the CI and release machinery, ahead of the real command
surface.
