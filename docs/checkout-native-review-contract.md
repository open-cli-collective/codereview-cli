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
5. Certify exact unchanged-blob/mode rename pairs against the pinned base and
   head Git trees, and publish the complete proof in run-owned
   `relocations.json`.
6. Run specialist reviewers against per-reviewer disposable workspaces, then at
   most one focused coverage-repair pass for each reviewer with unresolved
   assigned readable-file obligations.
7. Write complete typed coverage to run-owned `coverage.json` before posting,
   then run rollup from findings, reviewer failures, and complete coverage.

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
  relocations.json
  coverage.json
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
      <reviewer-id>-coverage-repair/
        repo/
    scratch/
      <reviewer-id>/
        cache/
        tmp/
      <reviewer-id>-coverage-repair/
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
- `workbench/repo/` is a clean pinned checkout at the PR head SHA, carrying
  `refs/heads/cr-review-head` at that SHA. The ref is load-bearing: the
  per-reviewer workspace is created with `git clone` from this directory, and
  git does not treat a directory without `refs/` as a repository. Only the head
  is given a ref, so the base commit is not transferred into reviewer
  workspaces when base is not an ancestor of head; reviewers are handed the
  provider-generated `diff.patch` and nothing in the pipeline resolves the base
  SHA inside the workspace. That is a decision, not an oversight -- add
  `refs/heads/cr-review-base` if a reviewer ever needs `git log base..HEAD`.
- `workbench/reviewers/<reviewer-id>/repo/` is a disposable reviewer checkout.
  Clone it through Git transport (`--no-local`) rather than copying the
  canonical object directory: background repacking can replace pack files
  during a local-copy clone. Each checkout has its own object database and
  receives only history reachable from the canonical refs.
- `workbench/scratch/<reviewer-id>/` holds reviewer-owned scratch, temp, and
  cache roots.
- A coverage-repair pass runs under the derived identity
  `<reviewer-id>-coverage-repair`, so a repaired run also has that reviewer
  checkout, that scratch root, and its own encoded agent log. Workspace
  preparation clears the directory it is given, so the repair must never reuse
  the primary reviewer's identity.
- Reviewer subprocesses receive scratch-local environment paths:
  - `TMPDIR`, `TMP`, and `TEMP` point at `scratch/<reviewer-id>/tmp`
  - `GOCACHE` points at `scratch/<reviewer-id>/cache/go-build`
  - `GOTMPDIR` points at `scratch/<reviewer-id>/tmp/go`
  - `XDG_CACHE_HOME` points at `scratch/<reviewer-id>/cache/xdg`

The workbench is run-owned, not cache-owned. Shared clone or fetch caches are a
possible future optimization but are not part of the correctness contract.

A successful run removes its `workbench/` tree when it reaches a successful
terminal state, after rollup and plan build, so retention no longer pins a full
checkout per run. A live run reaches that state when its outbox post succeeds;
failed, aborted, and incomplete runs retain the workbench for inspection, and
`data.keep_workbench: true` opts a run back into retention on success. The
benchmark caller-owned selection path (`cr benchmark select`) reclaims its
checkout on success under the same opt-out.

`workbench/metadata.json` is a versioned artifact for retained workbenches:
failed or errored runs, and successful runs with `data.keep_workbench: true`.
Schema version `2` records:

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

- a compact columnar `file_manifest` containing the full changed-file metadata
  rows in the provider's original order, plus assignment indices
- reviewer instructions
- reviewer-facing dossier content
- pinned workbench identity metadata

The manifest is input-only. Its columns are `path`, `old_path`, `status`,
`additions`, `deletions`, `hunk_count`, `binary`, `reviewable`, and
`verified_relocation`; each row retains the complete raw path strings and
statistics. A verified relocation is an explicit unique provider rename pair
whose pinned base/head entries are regular blobs with the same full object ID
and mode, with the old path absent at head and the new path absent at base.
Distinct certified moves may share a blob ID. The full sorted move proof and
manifest digest live only in `relocations.json`; reviewer prompts carry the
manifest digest, assigned move count, and assignment digest rather than
repeating old/new paths, object IDs, or modes. Selection, reviewer, and
coverage-repair prompts use compact JSON. Repeated assignment references use
zero-based row indices rather than repeating path arrays. The public selection
and findings schemas continue to use literal path strings.

For reviewer prompts, `scope_indices` is the authoritative set of paths the
reviewer may inspect and report for coverage. `file_indices` and
`allowed_file_indices` preserve the original assignment as context and do not
widen that scope. A reviewer may cite an otherwise out-of-scope removed or
renamed path only through the explicit `extra_citation_refs` cells derived
from the existing citation allowance. Coverage repair is restricted to its
scope path cells and receives no extra citation references. Rollup coverage
context carries counts and status rather than duplicate path arrays; durable
coverage and approval decisions continue to use the full local path data.

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
- Selection must also include a digest of the exact raw
  `dossier/raw/changed-files.json` bytes. This invalidates reuse when raw file
  metadata changes even if the scoped prompt is unchanged.
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

For a changed symlink assigned to a Pi reviewer, `cr_read` also accepts the
explicit `view="symlink"` option. That view reads a run-owned, digest-validated
artifact built from the pinned base/head Git trees; it returns the exact link
payload and lexical target status without following the link or reading the
destination body. Its output uses the same bounded range/continuation contract.
Payloads larger than the inspection cap are represented with their size and an
omission reason without fetching their contents, so the link payload remains a
skipped review obligation. An explicit empty payload and its zero-byte size are
preserved distinctly from an omitted payload. The default file view, search,
and list continue to deny symlink traversal.

Unsupported adapters must fail clearly. They must not silently fall back to
stuffed diffs or full file bodies.

## Reviewer Output Contract

Specialist reviewers must return structured output that includes:

- `findings`, each with severity, changed-file path, anchor, and body
- `inspected_files`, listing assigned changed files the reviewer actually inspected
- `skipped_files`, listing assigned changed files the reviewer intentionally did not or could not inspect
- `context_files`, listing safe pinned-head repository paths actually read as
  supporting context, including paths outside the assigned changed-file set
- an optional `relocation_assessment` with exact manifest and assignment
  digests, `path_impact_reviewed: true`, nonempty evidence paths, and a concise
  basis for assessing assigned certified moves without rereading their bodies
- `constraints`, listing material scope, context, or tool constraints

The relocation assessment must discuss imports and relative references,
workspace membership, build/CI/scripts, runtime assets/routes, guidance, and
ownership as relevant. Its evidence must be in `context_files` or
`inspected_files`, and must exist in the pinned head tree. It credits only the
reviewer's certified assigned moves. Explicit skips override relocation credit;
they represent unresolved coverage/path-impact obligations, not a requirement
to reread a certified move's body. Certified moves with valid impact review are
not to be falsely claimed as body-inspected. Changed residual files still
require actual inspection. Safe out-of-assignment paths may be preserved as
context without receiving assignment coverage; invalid, nonexistent, absolute,
traversing, or `.git` paths are rejected. Findings and citations remain subject
to the existing strict assignment and anchor allowlists.

Rollup receives compact reviewer coverage summaries derived from body
inspection, valid relocation-impact review, context, skips, failures, and typed
missing-file lists. `allowed_files` is assignment focus, not incomplete coverage
by itself. Isolated reviewer failures, unresolved skips or residuals, missing
reviewer results, and unassigned changed files are incomplete coverage and must
not turn into a clean approval silently.

Before the final post is prepared, `coverage.json` records the complete local
coverage collections, failures, constraints, relocation assessment records,
and manifest digest. Dry-run JSON exposes complete collections and artifact
paths. Public summaries use deterministic counts and at most five examples per
path list with explicit omitted counts; diagnostic prose is sampled to 500
Unicode runes. The complete details remain local in the run artifacts. Never
put private absolute paths in public provider bodies. Preserve the existing
finding-body clipping policy; do not add new clipping of finding bodies or
anchors, and never clip gate data. The final marker-bearing REST body is
preflighted at 60,000 UTF-8 bytes; an oversized body is rejected before an HTTP
write while local artifacts remain available. This conservative limit does not
assert a confirmed cause for any earlier provider rejection.

Coverage repair targets each assigned readable file not actually body-inspected
and not covered by a valid relocation-impact assessment, including omitted
files and explicit skips. It uses one focused workspace/session per reviewer;
the repair has its own assignment digest. Deleted, binary, and configured
generated-lockfile exclusions continue to apply. A repair merges findings and
coverage while preserving primary findings and primary tool evidence. A failed
primary required tool remains a gate even if a later response reports broad
coverage.

When incomplete coverage is what downgraded an approving review to a comment,
the rollup says so under an **Approval Withheld** heading, naming the reviewers
that produced no result, the coverage diagnostics behind any other incomplete
status, and every changed file no reviewer body-inspected or validly
impact-reviewed. Without it, a review that
approved and a review that found nothing but could not approve render
identically as a table of zeros. Re-running is not a remedy either: the focused
coverage-repair pass has already attempted each eligible unresolved readable
obligation once, so what the section names is what stayed incomplete after that
second look.

The section renders whenever that coercion fired, rather than deciding again
from the evidence, so it cannot disagree with the gate about whether coverage
was incomplete. `coverageStatusComplete` is the one classification of the status
enum, read by both the gate and the section, so a status neither knows fails
toward withholding approval and toward being explained.

Every reviewer with a non-complete status gets a line, so the section always
carries evidence. That includes a reviewer whose skipped file another reviewer
covered: the gate is evaluated one reviewer at a time while the uncovered-file
list is computed across the review, and in that state no file is uncovered yet
approval is still withheld. The section says so outright rather than
introducing a list and listing nothing.

The uncovered-file list is each reviewer's obligation minus what some reviewer
body-inspected or validly impact-reviewed. Obligation is the reviewer's scope, not its skip list: a reviewer
that failed carries a scope and no file lists, and one that omitted a file from
both lists carries the paths only in its diagnostic. A reviewer's skipped paths
are spelled out on its own line only where the unread list does not already
carry them.

Coverage uses two related scopes:

- readable files: all changed files in the workbench, **except generated
  dependency lockfiles** (`Cargo.lock`, `package-lock.json`, `go.sum`, and the
  well-known peers — see `isGeneratedLockfile`). Lockfiles are machine-written
  and reviewed, if at all, through the manifest change that produced them, so
  they are exempt from the coverage universe: neither a reviewer that skips one
  nor an unassigned lockfile counts as incomplete coverage. The exemption is
  applied in one place — the orchestrator's glob-coverage assigner does not
  force-assign a lockfile, and the coverage accounting drops lockfiles from both
  scope and inspected/skipped rows — so scope and coverage are drawn from the
  same set.
- assignment scope: `allowed_files` when present, otherwise `files` when the
  orchestrator supplied them, otherwise all changed files (lockfiles exempt, as
  above)

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
