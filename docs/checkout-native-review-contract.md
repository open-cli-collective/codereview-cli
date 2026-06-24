# Checkout-Native Review Contract

This document defines the target contract for the "avoid context stuffing"
review pipeline. It is the design boundary for the follow-on implementation
issues and should stay aligned with the durable LLM task model described in
`docs/llm-task-artifacts.md`.

## Goal

Large pull requests must be reviewable without stuffing full diffs or full file
bodies into LLM prompts. The review harness should instead prepare a pinned
checkout plus compact review artifacts, then let the orchestrator and
specialist reviewers inspect code on demand through bounded read-only tools.

## Runtime Sequence

The runtime sequence for checkout-native review is:

1. Fetch raw PR context and repo guidance from the base branch.
2. Run the existing gate and override checks.
3. Prepare review inputs:
   - pinned read-only workbench
   - durable discussion summary
   - final dossier artifacts
4. Run orchestrator selection from dossier/workbench inputs.
5. Run specialist reviewers against the read-only checkout.
6. Run rollup from findings, reviewer failures, and inspected coverage.

This order is load-bearing. Discussion summarization happens before final
dossier assembly, and the dossier is assembled before orchestrator selection.

## Artifact Model

All checkout-native review artifacts are owned by the review run and must live
under the run artifact directory so existing retention, `cr data prune`, and
`cr data purge` remove them automatically.

The target layout is:

```text
runs/<run-id>/
  diff.patch
  findings.json
  rollup.md
  llm-tasks/
  dossier/
    raw/
    summary/
    final/
    index.json
  workbench/
    repo/
    scratch/
    metadata.json
```

Notes:

- `llm-tasks/` keeps the existing durable structured-task artifacts.
- `dossier/raw/` holds fetched PR context that is useful to later deterministic
  or LLM-backed dossier assembly.
- `dossier/summary/` holds durable normalized discussion artifacts.
- `dossier/final/` holds the reviewer-facing dossier files used by the
  orchestrator and specialists.
- `workbench/repo/` is the pinned checkout at the PR head SHA.
- `workbench/scratch/` is the only writable directory that reviewer adapters may
  use.

The workbench is run-owned, not cache-owned. Shared clone or fetch caches are a
possible future optimization but are not part of the correctness contract.

`workbench/metadata.json` is a versioned durable artifact. Schema version `1`
records:

- `schema_version`
- `source_repo_root`
- `checkout_mode`
- normalized PR identity under `pr`
- pinned base and head refs
- `repo_path` and `scratch_path`
- sorted changed file paths
- `fingerprint_inputs`, which restates the semantic inputs downstream resumable
  tasks use to detect stale workbench state

`fingerprint_inputs` is intentionally redundant with the top-level metadata so
downstream tasks can fingerprint workbench semantics without re-deriving them
from the checkout tree ad hoc.

`SelectionOnly` and benchmark callers still own their artifact directory choice.
Checkout-native additions must work with caller-owned artifact roots instead of
assuming that every selector run is ledger-backed.

Workbench preparation must also define how normal fork-backed PR heads are
fetched and pinned. That fetch strategy belongs to the workbench implementation
issue, but checkout-native review must support ordinary GitHub PR shapes rather
than assuming same-repository branches only.

## Reviewer-Facing Context

Only include information that helps an agent understand the intended change or
evaluate changed code.

Reviewer-facing dossier context may include:

- PR title and body
- changed file paths with status and basic stats
- repo review guidance with provenance
- top-level PR comments that affect review judgment
- inline discussion summarized and anchored to file and line
- settled thread decisions
- unresolved human disagreement when it remains relevant

Reviewer-facing dossier context must not include:

- CI status, mergeability, draft state, approvals, requested reviewers
- session IDs, run IDs, retry state, cache state, ledger bookkeeping
- full diff hunks by default
- full base or head file bodies by default
- stale process chatter that does not improve review judgment

The current `needs_full_file_content` flag is legacy prompt behavior. It should
not participate in checkout-native reviewer execution.

## Reviewer Prompt Contract

Checkout-native specialist reviewers receive a compact prompt contract plus
bounded checkout access. The prompt payload is reviewer-facing context only:

- assignment metadata, including selected files and any `allowed_files`
- reviewer instructions
- reviewer-facing dossier content
- pinned workbench identity metadata

The prompt payload must not embed:

- raw diff hunks as reviewer input
- base/head file bodies
- harness/runtime selection fields such as model tier, resolved model IDs, or
  effort knobs

`needs_full_file_content` is deprecated in checkout-native mode. Legacy agents
that still declare it must receive checkout-readonly review behavior instead of
prompt stuffing, and agent-authoring guidance should treat checkout-native
review as the supported path going forward.

## Harness-Only State

The harness owns process and safety data that should stay out of reviewer
context unless a later issue proves an explicit need:

- durable task metadata and resume state
- provider and ledger session handles
- progress logging fields
- posting mode and outbox state
- gate classifications and approval override bookkeeping
- run IDs, retention metadata, and cleanup state

This separation is important because reviewer prompts should stay focused on the
code and the intent of the change, not the mechanics of the review run.

## Durable LLM Tasks

Checkout-native review continues to use the existing durable LLM task model.
The current load-bearing progress fields are:

- `task_id`
- `phase`
- `source`
- `agent_id`
- `model`
- `effort`
- `log_file`
- `resume_session_id`
- `task_status`
- `session_id`
- `validation_attempts`

Target task identities are:

- `dossier-discussion-summary`
- `orchestrator-selection`
- `reviewer-<encoded-agent-id>`
- `orchestrator-rollup`

The discussion summarizer feeds dossier assembly. The orchestrator selection
task depends on dossier/workbench inputs. Reviewer tasks depend on selection.
Rollup depends on selection and all reviewer tasks.

## Fingerprint Rules

The durable task fingerprint must change whenever the semantic input to a task
changes, even when the prompt only references artifact paths instead of
embedding their contents.

This is a contract requirement:

- Any task that reads dossier or workbench artifacts by path must include
  content digests for those artifacts in its `input_fingerprint`.
- Dependency task IDs alone are not sufficient when the prompt references
  generated files outside the prompt body.
- Resume must reject stale artifacts the same way it rejects stale prompt
  inputs.

Without artifact digests, a resumed task could reuse output for a changed
dossier or checkout while the prompt string remains unchanged.

## Reviewer Assignment Contract

The orchestrator selects reviewers and returns structured assignments.
Assignments may include:

- reviewer ID
- rationale
- suggested starting files or symbols
- optional `allowed_files`

`allowed_files` semantics are:

- empty: reviewer may inspect the full pinned checkout
- non-empty: reviewer is constrained to the listed paths

The orchestrator may narrow `allowed_files` for highly specialized reviewers,
but the default posture is full-checkout visibility.

## Adapter Contract

Adapters that participate in checkout-native review must expose a
checkout-readonly capability with these properties:

- read/search/git-diff style access to `workbench/repo/`
- write access only to `workbench/scratch/`
- bounded command timeouts
- bounded tool output
- explicit failure when the capability is unsupported

Unsupported adapters must fail clearly. They must not silently fall back to
stuffed diffs or full file bodies.

## Reviewer Output Contract

Specialist reviewers must return structured output that includes:

- `findings`, each with severity, changed-file path, anchor, and body
- `inspected_files`, listing assigned changed files the reviewer actually inspected
- `skipped_files`, listing assigned changed files the reviewer intentionally did not or could not inspect
- `constraints`, listing material scope, context, or tool constraints

Rollup receives compact reviewer coverage summaries derived from those fields.
`allowed_files` is intentional scope narrowing, not incomplete coverage by
itself. Isolated reviewer failures, skipped files, missing reviewer results, and
unassigned changed files are incomplete coverage and must not turn into a clean
approval silently.
