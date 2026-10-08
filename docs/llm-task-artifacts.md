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

`schema_version` is currently `4`. Version 2 added the relocation/context
coverage contract. Version 3 added the pinned symlink metadata digest and
inspection-contract version to reviewer, coverage-repair, and rollup resume
inputs. Version 4 adds bounded per-call reviewer tool telemetry, including the
explicit distinction between regular-file and symlink-metadata read views.
Schema versions 1, 2, and 3 are rejected for reuse and require rerunning the task,
so old successful Pi tasks cannot silently reuse output without this collection
contract. The schema version participates in the input fingerprint. Old
metadata is never treated as safe merely because the prompt or assignment
count appears unchanged.

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
  phase, model/effort, prompt, dependency task IDs, and every semantic artifact
  dependency. Reviewer and coverage-repair fingerprints include the relocation
  manifest digest, exact assigned relocation digest, pinned symlink metadata
  digest and inspection-contract version, and context-contract version. Rollup
  also includes the complete `coverage.json` content digest and pinned symlink
  metadata digest/contract version.
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
For a reviewer with a recorded result, explicit evidence from its primary
session with any status other than `succeeded` makes coverage `incomplete_tool`,
even if the result reports all assigned files as inspected. Incomplete coverage
clamps an otherwise approving review to `comment`. Successful tool evidence does
not itself prove complete coverage; the normal assigned-file coverage checks
also apply.

The lifecycle persists this evidence in metadata and restores it when loading
a cached task, so reusing successful output preserves the tool state used to
assess coverage and approval.

In schema version `4`, an absent `reviewer_tool_evidence` field means no
adapter-provided evidence is available. It is distinct from explicit
`not_invoked` evidence: absence does not trigger the tool-evidence coverage
check, but the normal coverage checks for skipped, missing, and unassigned
files still apply. Version 1, 2, or 3 metadata fails the schema check before any task
output can be reused.

### Bounded execution trace

Pi reviewer invocations also persist `reviewer_tool_evidence.trace`, version
`1`, source `pi_rpc`. This trace records only actual `tool_execution_start` and
`tool_execution_end` events for `cr_read` and `cr_diff`. Model tool-call text,
`inspected_files` claims, searches, and directory listings do not create file
read evidence. Other adapters, including subprocess adapters, leave evidence
absent; an absent trace means this per-call contract is unsupported/unavailable,
not that no calls happened. A present empty `calls` array means no supported
execution events were observed during that invocation.

Each call retains its exact bounded `call_id`, tool, observed start/end flags,
requested byte offset/limit, and `succeeded`, `failed`, or `incomplete` outcome.
Success requires matching start/end call identity and an explicit non-error
execution outcome. An unmatched end, missing end/identity/outcome, duplicate
identity, or mismatched tool cannot establish success. A stable
`provenance_issue` explains missing or ambiguous observations without copying
raw arguments, tool output, or failure text into the trace.

For `cr_read`, `path` is the unchanged canonical repository-relative argument.
Absolute, traversing, VCS metadata, noncanonical, or unavailable paths are
omitted with a provenance issue rather than normalized into another file's
evidence. This collection is independent of reviewer assignments: context
reads can be recorded without receiving assignment coverage. `read_view` is
`file` for an ordinary read or `symlink` for an explicit `view="symlink"`
request. Unavailable arguments or an invalid explicit view leave `read_view`
absent rather than assuming a file-body read. Symlink-view ranges cover pinned
metadata only: they do not read a destination body or a head regular-file body for a base-only
symlink transition. The trace does not duplicate pinned link payloads, digests,
target resolution, or payload-omission evidence from the symlink artifact.
Tool success alone does not establish that a non-omitted link payload was read.
`cr_diff` reads the fixed pinned diff and has no path parameter, so its entries are always
unattributed to individual files. Its success never fabricates a per-file read.

`output`, when present, is CR-extension-reported transport byte count and
truncation state. Absence means unknown. `output.truncated: false` does not mean
a complete file/diff was returned: the helper may have selected a bounded
range. Range headers in returned text are not parsed as provenance, because
repository content itself can contain those strings. Zero requested offset and
limit select the helper's default bounded read, not unbounded/full inspection.

The collector retains at most 256 calls and 64 KiB of combined identity/path
bytes. IDs are limited to 256 bytes and paths to 4096 bytes. Identity/path
strings are omitted instead of shortened into misleading identities.
`truncated` records collection limits, and `dropped_events` counts events
discarded at the record/identity budget. End events for already retained calls
can still complete those calls after the cap. `stream_complete` requires a
clean `agent_end`, parse, and process exit; a complete stream can still contain
incomplete calls. The trace has its own bound and survives RPC/stderr log caps.

Lifecycle metadata preserves the terminal invocation's trace for success,
isolated failure, and cache reload. Each provider invocation/validation retry
owns a separate trace; a fresh or resumed call does not inherit or union tool
evidence from an earlier invocation. Pi itself still does not support provider
session resume. Mutable trace snapshots are copied at response and lifecycle
boundaries.

This is collection-only telemetry. A successful call is not proof that the
reviewer read the entire file, understood it, or meaningfully inspected it.
The existing diff gate, symlink inspection contract, and coverage/approval
behavior are unchanged. The separate structured model-output schema remains
version `1`; only durable task metadata advances to version `4`. Per-file
reconciliation remains a separate opt-in follow-up; it must account for missing
or truncated evidence and distinguish assigned body inspection, relocation
impact assessment, and supporting context. `coverage.json` remains the existing
coverage artifact; the trace does not create a competing coverage format.

## Relocation and Context Coverage

Reviewer structured output may include `context_files` and an optional
`relocation_assessment`. Context paths are exact safe repo-relative paths that
the reviewer actually read from its pinned reviewer workspace; they are not
assignment coverage. The decoder rejects absolute, traversing, `.git`, or
nonexistent head-tree paths, and preserves valid path bytes without trimming
them into a different path.

An assessment can credit only certified relocations assigned to that reviewer.
It must carry the exact relocation-manifest and assignment digests, set
`path_impact_reviewed` to true, provide a nonempty basis and nonempty evidence
paths, and cite evidence present in `inspected_files` or `context_files` that
exists in the pinned head tree. It does not claim the moved file body was
inspected. Explicit `skipped_files` override any assessment for the same move.
Findings and citation validation remain independent and retain their strict
assignment/anchor allowlists.

Coverage stores body-inspected, context, relocation-reviewed, missing, skipped,
assessment, constraint, and failure collections in run-owned `coverage.json`.
Coverage repair targets each assigned readable unresolved obligation, not only
explicit skips. Rollup fingerprints the complete coverage artifact, and public
rendering uses bounded samples while retaining full local evidence.

## Reviewer Coverage Repair Tasks

After a successful primary reviewer task, the pipeline may run one focused
coverage-repair task for assigned readable files that remain unresolved: files
explicitly skipped, omitted from both inspected and skipped lists, or certified
relocations without a valid path-impact assessment. A valid assessment covers
only its exact assigned certified moves, with matching manifest/assignment
digests and nonempty evidence read from the pinned head tree. Explicit skips
override that assessment. Context paths are preserved separately and do not
count as assignment coverage.
Deleted and binary files are outside the repair set, as are files whose basename
is in the `generatedLockfiles` set (`Cargo.lock`, `bun.lockb`, `go.sum`, and the
rest of that map); a lockfile spelled outside it, such as `bun.lock`, is repaired
like any other readable file. Files outside the repair set remain covered by the
normal exemption or fail-closed rules. The repair is also skipped when the
primary session reports `reviewer_tool_evidence` whose `diff_status` is anything
other than `succeeded`: that session's own evidence already makes coverage
`incomplete_tool`, so the entry would stand regardless of what the repair
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

The primary findings are retained and repair findings are appended. Body
inspections, safe context paths, relocation-reviewed paths, assessment records,
and constraints are merged as separate collections. A primary unresolved file
is cleared only when the repair explicitly covers it by actual body inspection
or valid relocation-impact assessment. An explicit repair skip overrides its
assessment. For body inspection, the repair must report the file in
`inspected_files` and its own `reviewer_tool_evidence`, when present, reports
`succeeded`; explicit repair evidence with any other status contributes no
inspected files, so the skip stands. As with the primary session, absence does
not trigger the tool-evidence check, so a repair reporting no evidence keeps its
inspected files. The primary required-tool evidence remains authoritative for
the final gate: a valid repair cannot erase a primary failed-tool status.
Unresolved skips or omissions after the single repair continue to make coverage
incomplete. Complete local coverage collections are written to run-owned
`coverage.json` before posting; public summaries expose bounded samples while
retaining the full artifact.
The reviewer task dependency list passed to rollup includes both the primary
and repair task IDs, so their outputs, sessions, and coverage status are
merged before approval is decided. Tool evidence is not merged across the two
passes: coverage reads only the primary session's evidence, so a repair can
neither improve nor worsen it.

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

The full relocation proof is stored only in run-owned `relocations.json`;
`coverage.json` stores the complete reviewer coverage evidence and its manifest
digest. Public status text is bounded and never substitutes for these local
artifacts. A changed manifest, assigned move set, context contract, or coverage
artifact digest invalidates reuse even if the move count and human-readable
prompt happen to remain unchanged.

Raw invalid structured output is local artifact data. Public rollups may include
concise diagnostics, but they must not include raw failed model output.

## Prompt Budget Rejection Diagnostics

Prompt-size checks run before the corresponding provider or reviewer-workspace
start. A rejection writes a local diagnostic receipt to
`prompt-budget/<sha256(task-id)>.json` under the artifact root, including for
caller-owned `SelectionOnly` artifacts. The receipt uses schema version `1`
and records only the phase, total and limit byte counts, fixed allow-listed
top-level section sizes (unknown sections are aggregated as `other`), and the
envelope size needed to reconcile the total. Invalid JSON is counted once as
`unparsed_bytes`.

Receipts never contain task IDs, model names, repository paths, prompt text, or
session identifiers. The directory and file are written with `0700` and `0600`
permissions. A successful later check does not delete the last rejection
receipt; it is diagnostic history only and never grants permission to resume
or bypass a budget check. Normal artifact-root retention and purge remove the
receipt with the rest of the run artifacts.

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

## Request-Cost Observations (Collection Only)

Optional `request_cost_evidence` and `request_cost_checkpoint` fields supplement
schema 4 without changing output reuse or reviewer-tool coverage semantics. Only
the concrete direct OpenAI Responses adapter produces v1 request observations.
Unknown or malformed optional cost data is isolated in both metadata readers;
valid output and reviewer-tool evidence remain available. Schemas 1/2/3 retain
the existing rejection behavior. This change does not enable cost estimation.

Each observed adapter attempt, including transient retries and structured-output
corrections, gets a distinct ordinal. Usage and observed response identity are
captured independently of accepted output, including no-text, incomplete,
non-2xx and cancellation paths. Missing/null values remain unknown. Requested
and observed model/service tier are separate. Invalid counters are diagnostic
only. No prompt, raw output, refusal/error body, URL, header or credential is
copied into these observations.

The adapter-owned OpenAI client uses a fresh pinned standard HTTP transport,
disables redirects and clears the replay body. Caller-supplied clients remain
opaque. Only the actual canonical global Responses destination qualifies as
`official_global`; gateway/custom routes remain unverified. Entering `Client.Do`
means `possibly_dispatched`, including network errors; it does not prove a free
request. A pre-dispatch failure is recorded separately.

The lifecycle holds the same fail-fast advisory task lock across cache checking,
checkpoint updates, invocation and metadata, including no-run mode. Reset uses
that lock too. The bounded sidecar lives at
`llm-tasks/.request-cost/<trimmed-task-id-sha256>.json`, outside the task directory removed
by fingerprint resets and inside the same root covered by retention/purge.
Before dispatch it atomically records a pending generation. After return it
writes the final checkpoint before ledger insertion or task output/metadata.
If the pending write fails, no adapter call starts. A failed final write leaves
the on-disk pending marker as unknown. Cache loads never create generations or
ledger rows. Reset reconciles metadata observations with the sidecar before
removing metadata, retaining a known prefix even if the sidecar was lost.

Histories retain at most 64 attempts and 64 finalized generation descriptors,
16 fixed gap reasons and 256 KiB encoded checkpoints. A 4 KiB reserve keeps room
for pending/unknown state even at capacity. Prefixes, saturating omission counts
and gaps survive later success. The whole legacy metadata read is still
unbounded; only its isolated cost envelope and the sidecar reads are bounded.

Task-history completeness remains false: this slice does not establish a fresh
artifact-root accounting origin. An absent directory, empty ledger, new session
ID or successful latest request cannot establish it. Interrupted, legacy,
opaque, conflicting, missing and overflowing observations remain unknown.
Primary and repair histories remain separate detached values. Direct OpenAI
costs are not passed to the old aggregate estimator; Codex cannot borrow direct
API pricing. Existing compatibility token counters and reported-cost semantics
are not redefined as sums of the new request history.

Evidence and checkpoints have fixed-order, map-free compact JSON SHA-256
projections with explicit nulls, normalized empty arrays, sorted gap enums and
separate versioned digest domains. They bind scope, ordered attempts, generation
fingerprints, pending state and omission counts. Prices are excluded.

Two writes per generation do not preserve every pre-crash observation: a crash
during an invocation may lose that generation's already observed prefix. Its
pending marker makes the missing history explicit. The repository's atomic
writer uses rename without file/directory fsync; no power-loss durability or
provider-invoice completeness is claimed. Per-attempt durable checkpoints,
fresh-origin initialization, request-local pricing and pricing receipts require
separate implementation and verification before estimate activation.

Cost-bearing metadata uses compact JSON so its durable nested-envelope size
matches the collector/reader admission budget; unrelated legacy metadata keeps
its existing formatting. Recovered metadata-only generations contribute a known
lower-bound count, with missing finalized descriptors recorded as omissions and
gaps. Their fingerprints are not reconstructed by guessing.
