# LLM Task Artifacts

`cr review` treats each structured LLM call as a durable task. Selection,
reviewer, and rollup calls must be isolated from each other so one failed task
does not erase successful upstream work or force unrelated LLM sessions to run
again.

Task artifacts live under a run artifact directory:

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

`metadata.json` is the commit marker. Writers must publish it last, after any
validated output or raw failed-attempt payloads are written and after the ledger
session row exists when a provider session is available. Resume code must only
trust the final `metadata.json` name, never a temporary metadata file.

## Schema Version

`schema_version` is currently `1`. Bump it when changing any load-bearing field,
status value, fingerprint input, task identity, or resume rule in a way that
could make an in-flight run unsafe to resume.

Load-bearing metadata fields are:

- `task_id`: stable task identity. Current values are `orchestrator-selection`,
  `reviewer-<encoded-agent-id>`, and `orchestrator-rollup`.
- `phase`: task phase, such as `selection`, `reviewer`, or `rollup`.
- `dependency_task_ids`: task IDs whose completed state was included in this
  task input.
- `input_fingerprint`: hash of the task schema version, adapter, task identity,
  phase, model/effort, prompt, and dependency task IDs.
- `agent_id`: reviewer agent ID for reviewer tasks.
- `status`: one of `succeeded`, `failed_isolated`, or `failed_blocking`.
- `session_row_id` and `provider_session_id`: ledger/provider session handles
  used for run summaries and provider-level resume.
- `adapter`, `model`, `effort`, and `log_path`: execution context.
- `validated_output_path`: structured output to decode when reusing a succeeded
  task.
- `error`: sanitized diagnostic for failed tasks.
- `attempts`: failed validation attempts with attempt label, provider session
  ID, raw output path when present, and decode error.

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
- Load matching `succeeded` reviewer tasks instead of rerunning reviewers.
- Load `failed_isolated` reviewer diagnostics instead of rerunning those
  reviewers automatically.
- Rerun `failed_blocking` tasks and downstream phases.
- Fail with rerun guidance when metadata is missing required payloads, points to
  a missing ledger session, has the wrong schema version, or has a stale input
  fingerprint.

Raw invalid structured output is local artifact data. Public rollups may include
concise diagnostics, but they must not include raw failed model output.
