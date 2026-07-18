# Review Guidance And Run Artifacts

This document describes the user-facing conventions for repo review guidance,
generated review context, and cleanup behavior in `cr review`.

## Repo Review Guidance

Repo-owned review guidance may live in trusted repo-local agent definitions
under `.codereview/agents/`. Shared profile agent sources provide organization-
or developer-managed reviewers without requiring repositories to duplicate
them.

Those agent definitions can shape review behavior by:

- adding repository-specific reviewers
- narrowing which file patterns a reviewer applies to
- carrying repository-specific prompt guidance for those reviewers

Agent source precedence is profile sources, repo-local base-branch agents, then
explicit `--agents-dir` sources. Later definitions override matching agent IDs.
Repo-local definitions that remain after merging are required when applicable;
the orchestrator selects every applicable repo-local reviewer before choosing
from the shared profile and flag pool.

Any source may set `required_on_match: true` with `file_globs`. Those reviewers
are added deterministically whenever a glob matches a changed file, even if the
orchestrator omits them. An explicit maximum smaller than the combined set of
applicable repo-local and matching `required_on_match` reviewers fails instead
of silently dropping required coverage.

`cr review` does not load repo guidance from the PR head for the same review
run. It reads `.codereview/agents/` from the PR base branch, pins that source
to the resolved base SHA, and uses that pinned base-branch guidance throughout
the run.

That means a PR which changes `.codereview/agents/` does not change its own
review behavior unless the same guidance is already present on the base branch.

## Guidance Provenance In The Dossier

Checkout-native review writes reviewer-facing dossier artifacts under the run
artifact directory. The file `dossier/final/repo-guidance.md` records:

- that repo review guidance comes from `.codereview/agents/`
- the provenance label for the pinned base-branch source
- the trust-boundary note that PR-head guidance changes do not affect that run
- whether the base branch guidance source was available, missing, unreadable,
  or invalid

This file is intended for reviewers and operators who need to understand which
repo guidance influenced a review without reading pipeline code.

Missing base-branch guidance is normal: `cr review` falls back to viable shared
profile or flag agents. Unreadable or invalid repo guidance remains blocking
because it indicates that maintainers attempted to declare authoritative
review behavior that could not be honored.

Without an explicit positive `--max-agents`, all applicable repo-local reviewers
and all matching `required_on_match` reviewers run, and the orchestrator may
select up to five optional shared reviewers. A positive `--max-agents` is a hard
total cap; a value below that combined required set fails, and otherwise
optional shared reviewers fill the remaining capacity. A non-empty change with
no viable merged reviewers fails without posting a synthetic review.

## Reviewer-Facing Context

Reviewer-facing dossier files are the parts of generated context that help an
agent review the code change itself. This typically includes:

- PR intent
- changed file paths and basic change-map details
- repo guidance provenance
- relevant top-level comments
- inline discussion summaries anchored to files and lines

Reviewer-facing dossier files intentionally exclude harness bookkeeping such as:

- session IDs and provider session handles
- retry and resume bookkeeping
- posting mode and outbox state
- retention metadata
- other internal runtime fields that do not improve review judgment

## Run Artifacts And Cleanup

Checkout-native review stores dossier and workbench artifacts under the normal
review run artifact root:

```text
runs/<run-id>/
  dossier/
  workbench/
```

Because those directories live under the run artifact root, existing data
lifecycle commands clean them up automatically:

- `cr data prune` removes dossier and workbench directories for pruned runs
- `cr data purge` removes the entire data root, including dossier and workbench
  artifacts

No separate retention setting is required for dossier or workbench cleanup.
