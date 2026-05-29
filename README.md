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
- required status checks for `lint` and `test`
- no direct pushes
- squash merge only

## Development

```bash
make tidy
make lint
make test
make build
```

The current tree includes only a placeholder Go package so these checks
stay green until the real CLI is added.
