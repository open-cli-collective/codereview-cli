# Architecture Refactor Workstream

This document is the temporary context artifact for the architectural refactor
workstream tracked by GitHub issue #420. It exists to preserve the workstream's
intent across many single-issue implementation runs.

This is not a status tracker, execution log, or replacement for tests. It is a
small map of the invariants every child issue should preserve while the code is
being moved into clearer layers.

Durable repo-local guidance still belongs in `docs/development.md` and
`docs/architecture.md`. Treat this file as issue #420 context and sequencing
memory only; if it appears to introduce a standalone repo policy, update the
durable guide or this temporary context so they agree.

## Thesis

This workstream is not about moving code into smaller files. It is about turning
implicit orchestration behavior into explicit, reusable, testable layers while
preserving existing program behavior.

Each child issue should make the system easier for future agents and engineers
to reason about by creating real seams, enforcing dependency direction, or
consolidating duplicated lifecycle behavior.

## Source Of Truth

Code and executable tests are the source of truth.

Docs are a navigation layer. They should describe the current code, tests, and
runtime contracts accurately enough for future agents to find the right sources
quickly, but prose is not enforcement. A docs-only PR is insufficient for this
workstream unless it is explicitly retiring or correcting workstream
documentation after the real code/test change has landed.

## Non-Negotiable Invariants

- Every workstream PR must preserve the ability of the locally built `cr`
  binary to run the normal live review loop against the PR under review.
- Issue-specific acceptance criteria must prove the behavior touched by that
  issue, not merely prove that `cr review` still starts.
- Provider writes must flow through planned actions, the ledger, and outbox.
- Production structured LLM execution must flow through `internal/llmlifecycle`.
- Runtime model selection must flow through `internal/stagemodel`.
- Reviewer-facing context must not leak harness-only state such as run IDs,
  session IDs, retry bookkeeping, ledger/outbox state, or gate bookkeeping.
- Resume, retry, and failure recovery must improve or remain behaviorally
  equivalent after each issue.
- Commands should be composition and rendering layers, not homes for reusable
  application services.
- Public command behavior must not regress unless the regression is explicitly
  accepted and documented as an intentional behavior change.

## Real Progress Criteria

A child issue counts as architectural progress only when it does at least one of
the following:

- removes an invalid dependency direction
- creates a named seam with a fakeable interface
- consolidates duplicated lifecycle or recovery behavior
- adds a mechanical architecture guardrail

Tests prove the refactor; they are not the refactor unless the issue is
explicitly about harnesses or guardrails. Docs make the refactor legible; they
are not enforcement.

## Acceptance Pattern

Every child issue should answer these questions before implementation is
considered complete:

- What program behavior could regress?
- Which test proves it did not?
- Which failure mode could get worse?
- Which test proves recovery still works?
- Which architectural boundary now exists?
- What code or test guard prevents that boundary from leaking again?

For every workstream PR, also build the branch's local binary and use it in the
normal live review workflow. Use a stable named session for repeated review
passes, for example:

```bash
go build -o ./bin/cr ./cmd/cr
CR_BIN="$PWD/bin/cr"
CR_PR=$(gh pr view "$PR_NUMBER" --json url -q '.url')
CR_SESSION="issue-${ISSUE_NUMBER}-pr-${PR_NUMBER}"
"$CR_BIN" review "$CR_PR" --session "$CR_SESSION"
```

If a prior live review already approved the PR or left a marker that would
short-circuit the next dogfood pass, keep the same named session and add
`--rerun` for that follow-up pass so the current branch still exercises the live
review loop:

```bash
"$CR_BIN" review "$CR_PR" --session "$CR_SESSION" --rerun
```

For issues that do not primarily touch review behavior, this is a dogfood
guardrail, not the issue's only acceptance criterion.

## Harness And Guardrails

The first child issue established named executable surfaces for future
workstream changes:

- `internal/cmd/reviewcmd`: command-level `cr review` composition from
  config/profile/flags through rendering.
- `internal/pipeline`: real dry-run review composition with fake provider,
  fake LLM, real ledger, temp state layout, deterministic IDs, dossier,
  workbench, planned actions, and durable-task resume behavior.
- `internal/reviewruntime`: command-independent review runtime assembly,
  credential-store cleanup, provider/adapter construction, and PR-scoped GitHub
  App installation lookup.
- `internal/appruntime`: command-independent application runtime helpers such
  as retention-policy conversion and repository-root resolution.
- `internal/architecture`: mechanical dependency guardrails for command
  coupling, application-layer imports of command/UI packages, and keeping
  application runtime contracts out of command helpers.

Future child issues should extend these named surfaces when their seam could
regress review composition or dependency direction. They should not clone broad
assertions into new one-off tests.

## Navigation Map

Use these docs as maps to the relevant code and tests:

- `docs/development.md`
- `docs/architecture.md`
- `docs/checkout-native-review-contract.md`
- `docs/llm-task-artifacts.md`
- `docs/review-guidance.md`
- `docs/init-ux-contract.md`
- `docs/init-config-surface.md`

When one of these docs disagrees with code or executable tests, fix the code or
tests if they are wrong; otherwise fix the doc.

## Retirement Rule

This document should not become permanent project lore by accident.

At the end of issue #420, delete this temporary workstream document unless it
has become a stable design document. Anything truly durable should be distilled
into code, executable tests, architecture tests, `docs/architecture.md`, or the
contract docs listed above.
