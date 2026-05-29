# Contributing

This repository is intentionally minimal for now. The real CLI will be
added later, but the repo should already behave like a normal Go CLI
project.

## Working tree expectations

- Keep `main` protected.
- Keep CI green before merge.
- Prefer small PRs that map to a single change.
- Use squash merge so the history stays linear.

## Branch protection baseline

Configure the `main` branch with:

- required status checks: `build`, `test`, `lint`, `pr-title`, `identity-check`
- at least one approving review
- dismiss stale approvals on new commits
- require conversation resolution
- block direct pushes

## Local checks

```bash
make tidy
make lint
make test
make build
```
