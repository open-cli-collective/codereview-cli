# Architecture Decisions

This document records review-pipeline boundaries that are intended to stay
stable as the implementation evolves.

## LLM Execution Boundary

Every LLM action must flow through one durable execution boundary. Callers
should describe the stage, scope, prompt input, structured output contract, and
model requirements; the runner owns provider invocation, retries, resume checks,
telemetry, and persistence.

The durable runner must do a pre-flight lookup before calling the provider. If
a completed matching step already exists, it should reuse the stored structured
result. After a provider call succeeds, the runner must persist the structured
result and metadata before returning it to downstream code. Metadata includes at
least provider, adapter, model, effort, started/completed timestamps, duration,
token usage, cost, and provider session identifiers when available.

New LLM-backed pipeline components should not call provider adapters directly.
They should receive a fakeable runner interface in unit tests and should return
domain results rather than posting comments or mutating provider state.

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

The direct `config.ResolveModelTier` exception is config inspection and the
resolver implementation itself. The architecture guardrail is enforced by
`internal/architecture/model_resolution_test.go`.

## Git Provider Writes

Provider writes have one durable path: planned actions in the ledger followed
by outbox execution. Commands and domain analyzers should not post comments,
reply to review threads, resolve threads, submit reviews, or mutate provider
state directly.

This keeps markers, retries, reconciliation, idempotency, and resume behavior in
one place. New commands such as `cr respond` should produce planned thread
actions and let the reviewplan/ledger/outbox flow perform provider writes.

## Inline Thread Lifecycle

Inline PR discussion threads are domain input, not provider-specific prompt
data. The intended decomposition is:

- `internal/threadcontext` normalizes `gitprovider.InlineThread`, detects
  codereview-authored finding threads, detects latest human replies, strips
  shared markers, collapses resolved threads to the latest durable summary, and
  produces file-scoped reviewer context.
- `internal/threadanalysis` accepts normalized thread context and returns
  reusable domain decisions: thread ID, decision, reply body, summary, resolve
  flag, and rationale.

Resolved inline threads should not be reprocessed as full conversations on
every review. Their durable context is the latest summary comment. Reviewer
prompts should receive compact file-scoped summaries so agents avoid re-raising
issues that have already been discussed and resolved.

`cr review` and `cr respond` should share the same normalization, filtering,
model resolution, LLM execution, and action-planning components. `cr respond`
is a command-shaped reuse of the thread-response portion of the review
pipeline, not a separate posting system.

## Retention And Cleanup

Durable LLM steps, thread-analysis results, and artifacts must be owned by a
run and must be safe to delete through the existing data lifecycle commands.
Database rows should reference `runs(run_id)` with cascade delete semantics, and
large artifacts should live under the run artifact directory.

When the retention window elapses, normal prune/GC commands should remove these
results along with the rest of the run. If a user deletes retained state, a
future review may need to spend time and tokens recreating it; that is the
expected tradeoff for user-controlled local data retention.
