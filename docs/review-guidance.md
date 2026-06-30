# Review Guidance And Run Artifacts

This document describes the user-facing conventions for repo review guidance,
generated review context, and cleanup behavior in `cr review`.

## Repo Review Guidance

Today, repo-owned review guidance lives in trusted repo-local agent definitions
under `.codereview/agents/`.

Those agent definitions can shape review behavior by:

- adding repository-specific reviewers
- narrowing which file patterns a reviewer applies to
- carrying repository-specific prompt guidance for those reviewers

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

When trusted base-branch guidance is missing, unreadable, or invalid, `cr review`
does not continue with degraded repo guidance. It short-circuits normal reviewer
execution and submits a `request_changes` review explaining that the trusted
repo-local guidance could not be used.

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
