# Checkout-Native Review Contract

This document defines the target contract for the "avoid context stuffing"
review pipeline. It is the design boundary for the follow-on implementation
issues and should stay aligned with the durable LLM task model described in
`docs/llm-task-artifacts.md`.

## Goal

Large pull requests must be reviewable without stuffing full diffs or full file
bodies into LLM prompts. The review harness should instead prepare a pinned
checkout plus compact review artifacts, then let the orchestrator and
specialist reviewers inspect and verify code on demand inside bounded
disposable workspaces.

Pinned base/head review currently runs against same-repository PR heads.
Fork-backed heads require additional fetch/auth handling before that entrypoint
can review them explicitly.

## Runtime Sequence

The runtime sequence for checkout-native review is:

1. Fetch raw PR context and repo guidance from the base branch.
2. Run the existing gate and override checks.
3. Prepare review inputs:
   - clean pinned workbench
   - durable discussion summary
   - final dossier artifacts
4. Run orchestrator selection from dossier/workbench inputs, selecting every
   applicable repo-local reviewer before optional shared reviewers.
5. Run specialist reviewers against per-reviewer disposable workspaces.
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
    reviewers/
      <reviewer-id>/
        repo/
    scratch/
      <reviewer-id>/
        cache/
        tmp/
    metadata.json
```

Notes:

- `llm-tasks/` keeps the existing durable structured-task artifacts.
- `dossier/raw/` holds fetched PR context that is useful to later deterministic
  or LLM-backed dossier assembly.
- `dossier/summary/` holds durable normalized discussion artifacts.
- `dossier/final/` holds the reviewer-facing dossier files used by the
  orchestrator and specialists.
- `workbench/repo/` is a clean pinned checkout at the PR head SHA.
- `workbench/reviewers/<reviewer-id>/repo/` is a disposable reviewer checkout.
- `workbench/scratch/<reviewer-id>/` holds reviewer-owned scratch, temp, and
  cache roots.
- Reviewer subprocesses receive scratch-local environment paths:
  - `TMPDIR`, `TMP`, and `TEMP` point at `scratch/<reviewer-id>/tmp`
  - `GOCACHE` points at `scratch/<reviewer-id>/cache/go-build`
  - `GOTMPDIR` points at `scratch/<reviewer-id>/tmp/go`
  - `XDG_CACHE_HOME` points at `scratch/<reviewer-id>/cache/xdg`

The workbench is run-owned, not cache-owned. Shared clone or fetch caches are a
possible future optimization but are not part of the correctness contract.

`workbench/metadata.json` is a versioned durable artifact. Schema version `2`
records:

- `schema_version`
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

Schema version `1` remains readable for exact-byte workbench reuse. Its legacy
`source_repo_root` field is ignored because the invocation checkout is not a
workbench input; a valid reused v1 metadata file is not rewritten, preserving
durable task fingerprints.

`SelectionOnly` and benchmark callers still own their artifact directory choice.
Checkout-native additions must work with caller-owned artifact roots instead of
assuming that every selector run is ledger-backed.

Normal review entrypoints derive credential-free HTTPS remotes from the
validated PR identity and fetch the exact base and head commits. Same-host fork
heads are supported; cross-host heads are rejected before authenticated Git is
invoked. Pinned dry-run entrypoints retain their stricter same-repository rule.

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

Reviewer execution must not request stuffed full-file content in the prompt
payload.

## Reviewer Prompt Contract

Checkout-native specialist reviewers receive a compact prompt contract plus a
prepared reviewer workspace. The prompt payload is reviewer-facing context only:

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
that still declare it must receive checkout-access review behavior instead of
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

Progress may also include optional telemetry fields when the provider reports
them: `tokens_in`, `tokens_out`, `cache_read`, and `cache_create`. These fields
are observable breadcrumbs, not resume inputs.

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

The merged catalog preserves profile, repo-local, then explicit `--agents-dir`
precedence. A winning repo-local definition is marked
`required_if_applicable`; missing repo-local guidance falls back to the shared
catalog, while unreadable or invalid repo-local guidance blocks review.

Without a positive `--max-agents`, the orchestrator selects every applicable
repo-local reviewer and every matching `required_on_match` reviewer plus up to
five optional shared reviewers. A positive value is a hard total cap: a value
below that combined required set fails, and otherwise optional shared reviewers
fill the remaining capacity.

`allowed_files` semantics are:

- empty: reviewer assignment is broad over the changed-file set
- non-empty: reviewer assignment is focused on the listed paths

The orchestrator may narrow `allowed_files` for highly specialized reviewers,
but reviewer workspaces still expose the disposable checkout so verification
commands can run with normal repository context.

`allowed_files` is an assignment and coverage signal. It is not a sensitivity
boundary: reviewer prompts, adapters, and operators should assume the selected
reviewer can inspect the disposable checkout while performing its review.
Reviewer findings are still scoped to the assignment: `allowed_files` when
present, otherwise the selected `files`, otherwise all changed files.

## Adapter Contract

Adapters that participate in checkout-native review must expose reviewer
workspace support with these properties:

- read/search/git-diff style access to the reviewer workspace repo
- writes target the disposable reviewer workspace and scratch/temp/cache roots;
  `workspace_write` adapters enforce that boundary through their native sandbox
- bounded command timeouts
- bounded tool output
- explicit failure when the capability is unsupported

Reviewer workspace support has modes:

- `permission_bounded`: adapter/tool permissions and prompt contract allow
  reviewer workspace inspection and verification commands.
- `workspace_write`: adapter sandboxing allows writes inside the disposable
  reviewer workspace.

Provider implementations map these modes onto their native controls. Claude CLI
reviewers run with `Read`, `Write`, and `Bash` tools when a reviewer workspace is
provided, so deployments must treat reviewer subprocesses as executing inside a
trusted review workbench rather than an OS-enforced write boundary. Codex CLI
reviewers run with `workspace-write` and the reviewer checkout as their working
directory.

Pi RPC reviewers use `permission_bounded` mode. They run from the disposable
reviewer checkout with Pi's built-in tools disabled and one invocation-owned
extension that exposes only `cr_read`, `cr_search`, `cr_list`, and `cr_diff`.
Those tools delegate to CR's bounded read-only helper: repository paths reject
absolute paths, traversal, links/reparse points, and filesystem-boundary
crossings, while `cr_diff` reads the run's precomputed pinned diff artifact
instead of invoking Git or honoring repository/user Git configuration.
The reviewer prompt requires `cr_diff` before head-file inspection. CR reserves
space within the existing aggregate log cap for a compact `cr_diff` event
summary so operators can distinguish no invocation, failure, and completion.
Read/diff responses expose bounded byte ranges with deterministic continuation
offsets, and list/search omit VCS metadata such as `.git`. Per-tool output,
tool duration, and aggregate reviewer RPC/stderr logs are bounded without
limiting protocol parsing. Non-reviewer Pi tasks retain their tool-free scratch
working directory.

Unsupported adapters must fail clearly. They must not silently fall back to
stuffed diffs or full file bodies.

## Reviewer Output Contract

Specialist reviewers must return structured output that includes:

- `findings`, each with severity, changed-file path, anchor, and body
- `inspected_files`, listing assigned changed files the reviewer actually inspected
- `skipped_files`, listing assigned changed files the reviewer intentionally did not or could not inspect
- `constraints`, listing material scope, context, or tool constraints

Rollup receives compact reviewer coverage summaries derived from those fields.
`allowed_files` is assignment focus, not incomplete coverage by itself. Isolated
reviewer failures, skipped files, missing reviewer results, and unassigned
changed files are incomplete coverage and must not turn into a clean approval
silently.

Coverage uses two related scopes:

- readable files: all changed files in the workbench
- assignment scope: `allowed_files` when present, otherwise `files` when the
  orchestrator supplied them, otherwise all changed files

The coverage status values are:

- `complete_broad`: a reviewer without `allowed_files` covered its assignment
- `complete_constrained`: a reviewer with `allowed_files` covered that narrowed
  assignment
- `incomplete_skipped`: assigned files were skipped or not reported as inspected
- `incomplete_tool`: the fixed-diff tool was not invoked, did not complete, or failed
- `incomplete_failed`: an isolated reviewer failure or missing reviewer result
  prevented coverage
- `incomplete_unassigned`: changed files were not assigned to any selected
  reviewer
