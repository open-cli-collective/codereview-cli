# LLM Task Artifacts

`cr review` treats each structured LLM call as a durable task. Selection,
reviewer, and rollup calls must be isolated from each other so one failed task
does not erase successful upstream work or force unrelated LLM sessions to run
again.

Task artifacts usually live under a run artifact directory:

```text
llm-tasks/<encoded-task-id>-<hash>/
  metadata.json
  validated-output.json
  initial.json
  retry.json
```

`<hash>` is the first 12 hex characters of the SHA-256 of the raw task ID. It
keeps task IDs that differ only by letter case apart on case-insensitive
filesystems. A run resumed across the introduction of that suffix re-runs its
LLM tasks once, because cached artifacts sit under the old names; the stale
directories are inert because enumerators key on the task ID recorded in
`metadata.json`.

Two derivations of that directory disagree on surrounding whitespace:
`llmlifecycle.Paths.TaskDir` trims the task ID before encoding it, while
`runartifact.Paths.LLMTaskDir` trims only for its emptiness check and then
encodes the untrimmed ID. A padded task ID therefore resolves to two different
directories depending on the caller. Issue #593 tracks reconciling them; callers
should pass already-trimmed task IDs until then.

Raw failed-attempt files are named `<label>.json` in the task directory. The
current structured adapter labels are `initial` and `retry`, which produce
`initial.json` and `retry.json` when raw invalid output is available.
`validated-output.json` stores the accepted structured JSON after prose
recovery, not necessarily the provider's raw structured-output bytes.

`metadata.json` is the commit marker. Writers must publish it last, after any
validated output or raw failed-attempt payloads are written and after the ledger
session row exists when a provider session is available. Resume code must only
trust the final `metadata.json` name, never a temporary metadata file.

## Schema Version

`schema_version` is currently `1`. Adding a new task that reuses the existing
metadata shape does not require a schema bump, so the schema version stays at
`1` for the dossier summary and reviewer coverage-repair tasks.

Bump it when changing any load-bearing field, status value, fingerprint input,
task identity, or resume rule in a way that could make an in-flight run unsafe
to resume.

Load-bearing metadata fields are:

- `task_id`: stable task identity. Current values are `orchestrator-selection`,
  `reviewer-<encoded-agent-id>`, `reviewer-<encoded-agent-id>-coverage-repair`,
  `orchestrator-rollup`,
  `dossier-discussion-summary`, `thread-analysis-<thread-id>`, and
  `approval-override`.
- `phase`: task phase, such as `selection`, `reviewer`,
  `reviewer-coverage-repair`, `rollup`, or `dossier`.
- `dependency_task_ids`: task IDs whose completed state was included in this
  task input.
- `input_fingerprint`: hash of the task schema version, adapter, task identity,
  phase, model/effort, prompt, and dependency task IDs.
- `agent_id`: reviewer agent ID for reviewer tasks.
- `status`: one of `succeeded`, `failed_isolated`, or `failed_blocking`.
- `session_row_id` and `provider_session_id`: ledger/provider session handles
  used for run summaries and provider-level resume. `session_row_id` may be
  empty only for caller-owned no-run artifact roots such as `SelectionOnly` or
  the pre-run approval override classifier.
- `adapter`, `model`, `effort`, and `log_path`: execution context.
- `validated_output_path`: structured output to decode when reusing a succeeded
  task.
- `reviewer_tool_evidence`: optional adapter-provided reviewer tool state; see
  [Reviewer Tool Evidence](#reviewer-tool-evidence). It affects coverage and
  approval, unlike usage telemetry.
- `error`: sanitized diagnostic for failed tasks.
- `attempts`: failed validation attempts with attempt label, provider session
  ID, raw output path when present, and decode error.

Telemetry metadata fields are optional and non-load-bearing. They do not affect
resume eligibility, and older artifacts without them remain valid:

- `tokens_in`, `tokens_out`, `cache_read`, `cache_create`,
  `cache_create_5m`, `cache_create_1h`, `cost_usd`, and `speed`:
  provider-reported usage copied into run summaries when available. The
  cache-duration fields preserve Anthropic's billable write buckets;
  `cache_create` remains their compatibility aggregate. `speed` records the
  delivered execution tier. Missing, mixed, or unsupported speed tiers remain
  unpriced when the provider does not report cost.

When an adapter reports token usage but no billed cost, `cr` may show an
API-equivalent estimate for a model in its dated public-price table. Estimated
costs are marked `(est.)` and include the pricing-basis identifier used. They
do not represent subscription consumption or an invoice. Provider-reported
cost remains authoritative. A nonzero cache-create aggregate without its
duration split is left unpriced because the five-minute and one-hour rates
differ.

## Status Semantics

`succeeded` means the task produced validated structured output. Resume may
reuse the output only when the metadata schema and input fingerprint still match
the current task.

`failed_isolated` is for reviewer-local LLM failures while the caller context is
still valid. This includes structured validation failures and provider failures
after a task provider session has started. Provider start failures with no
session are treated as blocking because they can indicate auth, quota, or other
systemic adapter problems. The failed reviewer is treated as
dependency-satisfied for downstream rollup, and the rollup receives a
diagnostic. Sibling reviewers continue to run. A review with any isolated
reviewer failure must not approve; the final event is clamped to at least
`comment`.

`failed_blocking` means the task prevents dependent phases from safely running.
Selection and rollup failures are blocking. `reviewrun` is the sole owner of
live planner-error outcomes: blocking LLM failures leave fresh and resumed runs
`incomplete`; transient failures leave resumed runs `incomplete` but fail fresh
runs; terminal failures fail either kind; cancellation leaves the current
outcome untouched. The normal resume gate can therefore rerun only the failed
task and downstream work after an interruption or durable blocking failure.

Provider start/wait failures may have empty `attempts` because no structured
output existed. When a provider session ID is known, retry should seed the next
task call with that session if the adapter supports resume.

## Reviewer Tool Evidence

`reviewer_tool_evidence` records the adapter-provided state of the required
reviewer diff tool, `cr_diff`. Its `diff_status` is one of:

- `not_invoked`: the reviewer never invoked `cr_diff`.
- `incomplete`: the reviewer started but did not complete `cr_diff`.
- `succeeded`: the reviewer completed `cr_diff` without failure.
- `failed`: the reviewer observed a `cr_diff` failure.

`diff_diagnostic` is an optional diagnostic string. For example, this
`metadata.json` fragment describes a successful task with failed tool evidence:

```json
{
  "status": "succeeded",
  "reviewer_tool_evidence": {
    "diff_status": "failed",
    "diff_diagnostic": "fixed diff unavailable"
  }
}
```

Task success means validated structured output, not complete review coverage.
Repeated paths within `inspected_files` or `skipped_files` are treated as one
claim; they do not add coverage or trigger another review attempt. Paths outside
the reviewer's allowed assignment and paths claimed as both inspected and skipped
remain invalid. Scope-repair diagnostics identify zero-based array positions
without echoing the rejected path into the retry prompt.
For a reviewer with a recorded result, explicit evidence with any status other
than `succeeded` makes coverage `incomplete_tool`, even if the result reports
all assigned files as inspected. Incomplete coverage clamps an otherwise
approving review to `comment`. Successful tool evidence does not itself prove
complete coverage; the normal assigned-file coverage checks also apply.

The lifecycle persists this evidence in metadata and restores it when loading
a cached task, so reusing successful output preserves the tool state used to
assess coverage and approval.

In schema version `1`, an absent `reviewer_tool_evidence` field means no
adapter-provided evidence is available. It is distinct from explicit
`not_invoked` evidence: absence does not trigger the tool-evidence coverage
check, but the normal coverage checks for skipped, missing, and unassigned
files still apply. The schema, fingerprint, and payload requirements for
resume remain unchanged; absence alone does not establish that an artifact
is safe to reuse.

## Reviewer Coverage Repair Tasks

After a successful primary reviewer task reports assigned readable files as
skipped, the pipeline may run one focused coverage-repair task for that reviewer.
Deleted and binary files are outside the repair set, as are files whose basename
is in the `generatedLockfiles` set (`Cargo.lock`, `bun.lockb`, `go.sum`, and the
rest of that map); a lockfile spelled outside it, such as `bun.lock`, is repaired
like any other readable file. Files outside the repair set remain covered by the
normal exemption or fail-closed rules. The repair is also skipped when the
primary session reports `reviewer_tool_evidence` whose `diff_status` is anything
other than `succeeded`: merged evidence keeps the worse status, so the
`incomplete_tool` coverage entry would stand regardless of what the repair
inspected. A repair task uses the same pinned PR revision and reviewer agent as
its primary task, but has its own workspace, durable task artifacts, and ledger
session.

- `task_id`: `reviewer-<encoded-agent-id>-coverage-repair`, derived from the
  primary reviewer task ID.
- `phase`: `reviewer-coverage-repair`.
- `dependency_task_ids`: exactly the primary `reviewer-<encoded-agent-id>` task
  ID. The repair input fingerprint also includes its focused readable-file list
  and prompt dependencies.
- `validated-output.json` and `metadata.json` are written under the repair task
  directory. The repair has its own session row and usage telemetry; when the
  provider supports resume, the primary provider session ID seeds the repair
  request without making the two task identities interchangeable.

Resume applies the normal task rules: matching succeeded repair output is loaded
without another provider call, while a changed fingerprint or dependency fails
closed with rerun guidance. An isolated repair failure is retained as a reviewer
failure so rollup can preserve the primary result while keeping coverage
incomplete.

The primary findings are retained and repair findings are appended. Inspected
files are unioned, and a primary skipped file is cleared only when the repair
explicitly reports it in `inspected_files`; skipped files that remain skipped
continue to make coverage incomplete. The reviewer task dependency list passed
to rollup includes both the primary and repair task IDs, so their outputs,
sessions, tool evidence, and coverage status are merged before approval is
decided.

## Resume Rules

Resume starts at the first task that cannot be reused:

- Load a matching `succeeded` selection task instead of rerunning selection.
- Load a matching `succeeded` dossier summary task instead of rerunning
  discussion summarization.
- Load matching `succeeded` reviewer tasks instead of rerunning reviewers.
- Load `failed_isolated` reviewer diagnostics instead of rerunning those
  reviewers automatically.
- Rerun `failed_blocking` tasks and downstream phases.
- Fail with rerun guidance when metadata is missing required payloads, points to
  a missing ledger session, has the wrong schema version, or has a stale input
  fingerprint.

Raw invalid structured output is local artifact data. Public rollups may include
concise diagnostics, but they must not include raw failed model output.

## Dossier Summary Task

`dossier-discussion-summary` is the durable LLM task that converts raw PR
discussion artifacts into reviewer-facing normalized summary artifacts.

- `task_id`: `dossier-discussion-summary`
- `phase`: `dossier`
- prompt input: bounded raw discussion projection from
  `dossier/raw/top-level-comments.json` and `dossier/raw/inline-threads.json`
- validated output: normalized summary JSON written both to the task artifact
  directory and to `dossier/summary/discussion.json`

For normal `cr review` runs, the dossier summary task executes after run
allocation and persists a normal ledger-backed session row.

For `SelectionOnly`, the caller owns the artifact root and no review run is
allocated. In that scoped mode:

- the cached task may still be loaded from task metadata plus validated output
- `provider_session_id` may still be present for provider-level resume context
- `session_row_id` may be empty
- loading the cached task must not require a ledger session lookup

This no-run behavior is intentionally limited to caller-owned artifact roots;
the normal run-backed durable task model remains unchanged for full reviews.

## Thread Analysis Tasks

`thread-analysis-<thread-id>` tasks classify one normalized inline discussion
thread and return a reusable decision, reply body, summary, resolve flag, and
rationale.

For `cr respond`, these tasks are run-owned. Successful analyses persist normal
ledger-backed sessions and are reused on retry. A normal `cr respond` invocation
resumes the latest incomplete response run for the same PR head, base, profile,
posting identity, and post mode. If analysis completed but planning or posting
was interrupted, rerun loads the persisted thread-analysis task instead of
calling the LLM again; if planned actions already exist, rerun continues through
the ledger/outbox post phase instead of replanning. Use `cr respond --rerun` to
start a fresh response attempt and leave the incomplete attempt untouched. If
the normalized thread input changes under the same task directory, the lifecycle
runner fails closed with rerun guidance instead of overwriting the prior task.

## Approval Override Task

`approval-override` is a pre-run classifier that detects explicit author
requests to approve without another full review pass.

The gate runs this before a review run may exist, so it uses the caller-owned
no-run lifecycle mode. Classifier failures are non-blocking: the gate warns and
continues with normal review. Successful and failed classifier task metadata
still lives under the prospective run artifact root so provider-session resume
and local artifact inspection use the same lifecycle shape.
