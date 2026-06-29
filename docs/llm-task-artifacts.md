# LLM Task Artifacts

`cr review` treats each structured LLM call as a durable task. Selection,
reviewer, and rollup calls must be isolated from each other so one failed task
does not erase successful upstream work or force unrelated LLM sessions to run
again.

Task artifacts usually live under a run artifact directory:

```text
llm-tasks/<encoded-task-id>/
  metadata.json
  validated-output.json
  initial.json
  retry.json
```

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
`1` for the dossier-phase addition in this slice.

Bump it when changing any load-bearing field, status value, fingerprint input,
task identity, or resume rule in a way that could make an in-flight run unsafe
to resume.

Load-bearing metadata fields are:

- `task_id`: stable task identity. Current values are `orchestrator-selection`,
  `reviewer-<encoded-agent-id>`, `orchestrator-rollup`,
  `dossier-discussion-summary`, `thread-analysis-<thread-id>`, and
  `approval-override`.
- `phase`: task phase, such as `selection`, `reviewer`, `rollup`, or
  `dossier`.
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
- `error`: sanitized diagnostic for failed tasks.
- `attempts`: failed validation attempts with attempt label, provider session
  ID, raw output path when present, and decode error.

Telemetry metadata fields are optional and non-load-bearing. They do not affect
resume eligibility, and older artifacts without them remain valid:

- `tokens_in`, `tokens_out`, `cache_read`, `cache_create`, and `cost_usd`:
  provider-reported usage copied into run summaries when available.
  `tokens_in`, `tokens_out`, `cache_read`, and `cache_create` are also copied
  into durable progress breadcrumbs when available.

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
Selection and rollup failures are blocking. Once a run exists, blocking LLM task
failures leave the run `incomplete` so the normal resume gate can rerun only the
failed task and downstream work.

Provider start/wait failures may have empty `attempts` because no structured
output existed. When a provider session ID is known, retry should seed the next
task call with that session if the adapter supports resume.

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
