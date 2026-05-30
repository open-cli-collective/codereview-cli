# codereview-cli

Open CLI Collective code review CLI.

The review pipeline is still pending. The current binary provides the Cobra
root, version command, and command wiring seam that future review commands will
attach to.

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

The tree currently holds a scaffold `cr` binary (`cmd/cr`) with a Cobra root and
version command, plus the CI and release machinery, ahead of the review command
surface.
