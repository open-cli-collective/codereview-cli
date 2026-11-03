# Review Lifecycle

This document is the canonical end-to-end lifecycle for `cr review`. It covers
the resumable reviewer sessions and early thread checkpoints introduced by
issue #529. Task artifact details remain in
[llm-task-artifacts.md](llm-task-artifacts.md).

## State ownership

| State | Scope | Authority |
|---|---|---|
| Review run, findings, and planned actions | PR head/base, profile, posting identity, attempt | Local ledger and run artifacts |
| Reviewer cohort and reviewer provider sessions | PR, profile, posting identity | Local ledger |
| Default orchestrator session | PR, profile, posting identity | Local named-session row |
| Explicit `--session` orchestrator session | Supplied name | Local named-session row |
| Posted thread summary | Posting identity and inline thread | PR marker; local action/task state is fallback until posting succeeds |
| Posted review actions | Run and action marker | PR state reconciled with the local outbox |

The reviewer cohort is ordered and fixed after its first selection. Each member
stores its agent ID, broad or scoped assignment, runtime compatibility fields,
and latest provider session ID. The explicit `--session` flag never changes the
reviewer cohort's PR scope.

## First run

1. Resolve the PR, changed files, discussion, reviewer catalog, and runtime.
2. Run selection on the orchestrator provider session.
3. Persist the selected reviewer cohort before starting reviewers.
4. Analyze eligible human replies sequentially on the same orchestrator
   session.
5. Persist thread reply and resolution actions, then run the partial outbox.
   Replies are attempted before resolutions. The run remains open.
6. Give every active reviewer the analyzed discussion outcome and actual early
   action status (`posted`, `pending`, or dry-run-only), then run reviewers.
7. Run rollup on the latest orchestrator provider session, merge final planning
   around the early action IDs and statuses, and run the final outbox.

A scoped cohort member whose rebased assignment is empty remains in the cohort
but does not receive a no-op LLM call.

## Later runs and `--rerun`

Plain follow-up runs and `--rerun` both load the original PR-scoped cohort,
validate every member against the current catalog and reviewer runtime, and
deterministically rebase assignments onto the current changed files. Selection
does not run again. Active reviewers resume their exact saved provider session
when its conversation still exists; a saved session whose conversation is gone
falls back to a fresh conversation instead of failing the reviewer (see the
failure behavior below).

The one way a reused cohort grows is `required_on_match`. Every catalog
reviewer marked `required_on_match` whose globs match a current changed file
runs with all of its matching files in its assignment, as a fresh selection
would schedule it. A required reviewer the saved cohort lacks — because a later
push touched files it must see, or because it joined the catalog after the
first review — is appended to the cohort and persisted before reviewers start,
so its provider session is recorded and resumed on later passes. A required
reviewer already in the cohort keeps the files rebasing gave it and gains every
matching file, rather than leaving new matches to whichever member rebasing
reached first.

`--rerun` changes local gate behavior only: it bypasses approval, override,
resume, and marker gates to allocate a new review attempt. It does not select a
new cohort, and it starts new provider conversations only through the same
missing-conversation fallback.

Reuse fails with `--fresh-session` guidance when a cohort member is missing or
runtime-incompatible, `--max-agents` is smaller than the saved cohort plus any
required reviewers joining it, joining required reviewers would change the
saved members' fast-mode setting, or a changed file is coverable by a current
catalog agent (broad with no file globs or matching a file glob) but neither a
saved cohort member nor a joining required reviewer takes it. The coverage
check runs on the final reused selection, after required reviewers join.
Generated lockfiles are exempt from it, as they are from reviewer coverage. If
no current catalog agent can cover a changed file, reuse leaves it unassigned
and the downstream coverage gate reports `incomplete_unassigned` without
forcing a fresh session.

## `--fresh-session`

`--fresh-session` clears the active PR-scoped reviewer cohort for this review,
does not resume the orchestrator provider session, runs selection again, and
persists the replacement cohort. It can be combined with `--rerun` when both
gate bypass and new reviewer/orchestrator conversations are required.

## Interrupted-run recovery

Structured LLM task metadata resumes an interrupted task inside the same run.
If interruption occurs after early thread actions are persisted, those actions
are valid partial planning state: recovery retries/reconciles them and continues
the remaining planning phases. Final planning preserves their IDs, attempts,
posted state, and pending errors.

A posting-identity-authored `codereview:thread-summary` marker suppresses repeat
thread analysis. A newer human reply after that marker reopens the thread. Local
task and action state suppresses duplicate work only while the marked reply has
not reached the PR.

## Failure and idempotency behavior

- Reviewer-local LLM failures remain isolated and retain any provider session
  ID reported by the failed attempt.
- A resume or fork whose provider conversation no longer exists is retried once
  as a new conversation instead of failing the task. Session reuse saves cost and
  carries prior context; neither is required for a correct review, so a cleaned-up
  conversation degrades rather than losing the reviewer.
- Early posting failures remain pending and do not prevent reviewer execution.
  Final posting retries them.
- A moved head or base stops checkpoint posting before reviewer work.
- Outbox reconciliation checks markers before every dispatch, so a process
  crash after a provider write but before the local update does not duplicate
  the reply or final review.
- GitHub reads and `git fetch` retry 5xx, 429 and connection failures with
  jittered backoff and honor `Retry-After`. A 404 fails immediately.
- A write that fails retryably during the final outbox is checked against the PR
  after a settle wait, marked posted if its marker is there, and otherwise resent,
  up to three sends. Once one action uses up its sends, later actions in that run
  get one send each, so an outage cannot multiply the wait. Whatever is left stays
  pending, the run exits 5, and the next `cr review` reconciles it. Duplicates remain
  possible only if GitHub applies a write after the settle wait and the marker check.
  GitLab declares no processing limit, so a GitLab write that fails retryably gets one
  send and stays pending for the next `cr review` to reconcile.
- A run that fails renders `Status: error` and the cause on stdout, or the same
  record as JSON, as well as the error on stderr and a non-zero exit code.
- Thread replies are ordered before resolutions, and a resolution is not sent
  until its reply is posted or reconciled.
- Completed summary markers on the PR outrank stale local fallback state.

`cr sessions` lists and manages named orchestrator sessions only. Reviewer
cohorts are automatic PR state and are replaced through
`cr review --fresh-session`.
