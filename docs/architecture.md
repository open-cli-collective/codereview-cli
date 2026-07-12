# Architecture Decisions

This document records review-pipeline boundaries that are intended to stay
stable as the implementation evolves.

## Durable LLM Execution Boundary

All production structured LLM actions must flow through
`internal/llmlifecycle`. Callers describe the task ID, phase, prompt input,
structured output contract, model/effort, artifact paths, and run/session
scope. The lifecycle runner owns provider invocation, structured-output retry,
provider-session resume, pre-flight reuse, task metadata, accepted-output
artifacts, session persistence, progress breadcrumbs, and failure
classification.

The final commit marker for a task is
`llm-tasks/<encoded-task-id>/metadata.json`. Writers must publish validated
output or failed-attempt payloads first, persist the ledger session row when
the task is run-owned, and write metadata last. Resume code must trust only the
final metadata path, never temporary files or partial payloads.

New LLM-backed components must not call `internal/llm` structured helpers
directly. They should call `llmlifecycle` through explicit, fakeable
dependencies in unit tests and should return domain results rather than
posting comments or mutating provider state.
`internal/architecture/llm_lifecycle_test.go` enforces that direct structured
helper calls stay inside `internal/llm` and `internal/llmlifecycle`. Direct
provider-adapter calls should also stay behind `llmlifecycle` for production
structured tasks; code review owns that broader boundary until a stronger
static guardrail exists.

Most lifecycle tasks are run-owned and must have a matching ledger session row
when a provider session is available. Caller-owned no-run tasks are allowed only
where no review run exists yet, such as `SelectionOnly` and the pre-run approval
override classifier. Those tasks may reuse artifact metadata without a ledger
session row, but they still use the same metadata schema and lifecycle runner.

## Stage Model Resolution

Runtime model choice must be resolved through `internal/stagemodel`. Code that
executes an LLM stage must not hard-code model IDs and must not call
`config.ResolveModelTier` directly.

`stagemodel.ResolveStageModel` is the single runtime path from profile
preferences and command overrides to a concrete model and effort. The request
must include the named stage, requested tier, default effort, and any explicit
operator override. The resolver applies user profile `llm.model_map` values,
built-in provider defaults, and configured tier floors before returning the
concrete runtime choice.

This boundary exists so model catalog data, provider capabilities, token costs,
and profile-level tier floors can be added without touching individual review
stages. Runtime hard-coding bypasses user preference and is a bug.

Reviewer `agent.model_id` is an exact provider-specific model override. It must
still enter runtime execution through `stagemodel.ResolveStageModel` as a model
override rather than bypassing the resolver, but it intentionally bypasses the
tier map because the agent author selected a concrete model.

The direct `config.ResolveModelTier` exception is config inspection and the
resolver implementation itself.
`internal/architecture/model_resolution_test.go` enforces that direct
`config.ResolveModelTier` calls stay inside approved packages. Hard-coded
runtime model IDs remain a code-review concern until model-catalog guardrails
exist.

## Git Provider Writes

Provider writes have one durable path: planned actions in the ledger followed
by outbox execution. Commands and domain analyzers should not post comments,
reply to review threads, resolve threads, submit reviews, or mutate provider
state directly.

This keeps markers, retries, reconciliation, idempotency, and resume behavior in
one place. New commands such as `cr respond` should produce planned thread
actions and let the reviewplan/ledger/outbox flow perform provider writes.

## Authenticated Git Reads

`internal/gitexec` is the narrow transport adapter for non-interactive Git
commands. The application composition roots construct it from the configured
repository/read provider's refreshable token source and inject its command
function into review and selection pipelines. Authentication is host-scoped
and process-only; posting/reviewer credentials are not checkout credentials.

The pipeline validates the requested, fetched, base, and head repository
identities before invoking Git. `internal/workbench.RunPreparer` then owns only
run-workbench creation, exact-commit fetching, validation, reuse, and metadata.
It does not inspect the caller's current directory or local Git origin.

## Command And Review Harness Guardrails

Command packages should stay thin: parse args and flags, load runtime
dependencies, call typed helpers, render through the view layer, and return
errors. Feature command packages should not import sibling feature command
packages; shared command infrastructure belongs in packages such as
`internal/cmd/cmderr`, `internal/cmd/cmdruntime`, `internal/cmd/exitcode`, and
`internal/cmd/root`. Command-independent helpers used to compose review and
response application runtimes, such as retention-policy conversion and
repository-root resolution, belong in `internal/appruntime` rather than
`internal/cmd/cmdruntime`. Review lifecycle runtime assembly belongs in
`internal/reviewruntime`; `cr review` and `cr respond` command packages should
construct `reviewruntime.OpenRequest` values and keep CLI-only validation,
rendering, config-path selection, and error mapping at the command boundary.

Application packages outside `internal/cmd` and `internal/view` should not
depend on Cobra, command packages, or view packages. Those packages should
return typed domain data so command and view code remain replaceable shells.
`internal/architecture/command_boundaries_test.go` enforces these dependency
directions with narrow allowances for command-tree integration tests and keeps
review/response application runtime contracts out of `internal/cmd/cmdruntime`.

Review behavior should be protected through named acceptance harnesses rather
than cloned broad assertions. The command-level harness verifies `cr review`
composition from config, profile, flags, runtime setup, and rendering. The
pipeline harnesses verify review composition and durable-task resume with fake
provider, fake LLM, real ledger, temp state layout, deterministic IDs, dossier
context, workbench preparation, planned actions, prompt inputs, and preserved
task metadata.

## Inline Thread Lifecycle

Inline PR discussion threads are domain input, not provider-specific prompt
data. The intended decomposition is:

- `internal/threadcontext` normalizes `gitprovider.InlineThread`, detects
  codereview-authored finding threads, detects latest human replies, strips
  shared markers, collapses provider-resolved threads to the latest sanitized
  comment, recognizes CR-authored settled summary replies when provider
  resolution is unavailable, and produces file-scoped reviewer context.
- `internal/threadanalysis` accepts normalized thread context and returns
  reusable domain decisions: thread ID, decision, reply body, summary, resolve
  flag, and rationale.

Settled inline threads should not be reprocessed as full conversations on every
review. Provider-resolved context is the latest sanitized comment on a thread
whose provider state is resolved. CR-settled context is the latest CR-authored
inline reply carrying a valid `codereview:thread-summary` marker when no newer
human reply follows it, even if the provider still reports the thread as
unresolved. Reviewer prompts should receive compact file-scoped summaries for
both sources so agents avoid re-raising issues that have already been discussed
and settled, while command output keeps provider resolution distinct from
CR-settled cache metadata.

`cr review` and `cr respond` should share the same normalization, filtering,
model resolution, LLM execution, and action-planning components. `cr respond`
is a command-shaped reuse of the thread-response portion of the review
pipeline, not a separate posting system.

## Retention And Cleanup

Durable run-owned LLM tasks, thread-analysis results, and artifacts must be
owned by a run and must be safe to delete through the existing data lifecycle
commands. Database rows should reference `runs(run_id)` with cascade delete
semantics, and large artifacts should live under the run artifact directory.

When the retention window elapses, normal prune/GC commands should remove these
results along with the rest of the run. If a user deletes retained state, a
future review may need to spend time and tokens recreating it; that is the
expected tradeoff for user-controlled local data retention.

Caller-owned no-run task artifacts must live under the configured data root or
an explicit caller artifact root. If no run is eventually allocated for that
artifact root, the directory is treated as orphaned local data and must be safe
for `cr data purge` to remove.
