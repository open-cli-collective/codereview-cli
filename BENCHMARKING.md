# Benchmarking

`cr benchmark` runs repeatable dry-run reviews across a matrix of review
configurations and pull requests. The harness is for measuring behavior, cost,
and operational characteristics. It does not grade review quality by itself.

## Vocabulary

Use these terms consistently in suite files, command output, artifacts, and
discussion:

| Term | Meaning |
|------|---------|
| Suite | A collection of benchmark cases and candidates. |
| Case | One pull request to run against, plus optional metadata such as expected observed base/head SHAs or anchors. |
| Candidate | One review configuration to try against each selected case. |
| Run | One candidate executed on one case. |

A candidate is not a suite. A candidate is the review configuration under test:
the base profile plus explicit stage recipes and optional max-agent and
concurrency overrides. A suite is the container that combines candidates and
cases into a matrix.

Profiles remain the account and execution context. They provide Git host/auth,
reviewer identity, LLM provider/auth/adapter, configured agent sources, and
review policy. Candidate stage recipes only adjust dry-run review runtime
behavior. Candidate `stages.reviewers.agent_dirs` values are passed as
additional trusted agent sources; they follow normal `cr review --agents-dir`
precedence after profile and repo-local base-branch sources.

## Directory Conventions

Use repo-local benchmark suites when they are safe to share:

```text
.codereview/benchmarks/
```

Use private local state for local-only cases, generated results, and scratch
analysis:

```text
.cr-bench/
```

Generated results should be ignored by default in repositories that run
benchmarks. They can contain private diffs, model output, stderr, local paths,
artifact paths, profile names, model/provider metadata, and usage details.
`cr benchmark run` and `cr benchmark select` do not create or update `.gitignore`; add the rule
manually when the repository should keep benchmark results private. A typical
repository ignore rule is:

```gitignore
.cr-bench/
```

The default `run` output path is:

```text
.cr-bench/results/<suite-id>/<timestamp>/
```

The default `select` output path keeps selector-only runs separate:

```text
.cr-bench/results/<suite-id>/select/<timestamp>/
```

Path timestamps are UTC, sortable, and Windows-safe. They do not contain colons:

```text
2026-06-03T184512Z
```

JSON artifacts store full RFC3339 timestamps:

```text
2026-06-03T18:45:12Z
```

## Suite Schema

Prefer the canonical `stages.reviewers.agent_dirs` field for candidate agent
directory lists. The loader currently accepts the draft alias
`stages.reviewers.agents_dir` for compatibility and does not emit a deprecation
warning, but new suites should use `agent_dirs`. A candidate cannot set both
names.

```yaml
suite:
  id: oss-model-cost-check
  name: OSS model cost check
  version: 1

candidates:
  - id: claude-sonnet-medium
    profile: work-anthropic
    stages:
      selection:
        model: claude-sonnet-4-6
        effort: medium
        prompt: prompts/selection-v1.md
      reviewers:
        model: claude-sonnet-4-6
        effort: medium
        agent_dirs:
          - ../agents
      synthesis:
        model: claude-sonnet-4-6
        effort: low
        prompt: prompts/synthesis-v1.md
    max_agents: 5
    max_concurrency: 5

  - id: kimi-low
    profile: work-fireworks
    stages:
      selection:
        model: moonshotai/Kimi-K2
        effort: low
      reviewers:
        model: moonshotai/Kimi-K2
        effort: low
        agent_dirs:
          - ../agents
    max_agents: 5

cases:
  - id: merged-security-pr
    pr: https://github.com/OWNER/REPO/pull/123
    review_base_sha: 1111111
    review_head_sha: 2222222
    without_discussion: true # default for pinned cases; false opts into saved sessions
    expected_base_sha: abc1234
    expected_head_sha: def5678
    anchors:
      - id: missing-auth-check
        file: internal/api/users.go
        side: RIGHT
        lines: [42, 45]
```

IDs must be unique within their list and use letters, numbers, underscores, or
hyphens. PRs must be GitHub pull request URLs. `review_base_sha` and
`review_head_sha` are optional, but must be set together when present; they pin
the exact base/head commit pair that `cr review --dry-run` evaluates instead of
the PR's current branch state. Expected SHA fields are optional baseline
metadata for downstream reports and graders; they do not change what gets
reviewed. All SHA fields must be non-empty 7 to 64 character hexadecimal SHAs
when present.

`without_discussion` is an optional per-case boolean for full-pipeline
`benchmark run`. It defaults to `true` when both review SHAs are pinned, and
`false` for unpinned cases. Setting it to `true` requires both review SHAs.
Discussion-free runs do not resume or overwrite saved orchestrator/reviewer
sessions or the PR's reviewer cohort, so candidates do not inherit earlier
reviews. Both subprocess and `--in-process` runs apply the same setting.
Explicit `false` opts into the normal saved-session behavior, including for
pinned cases. Expected SHA metadata alone does not enable discussion-free mode.

For cross-version comparisons with `--cr-bin`, isolated cases require a binary
that supports `cr review --without-discussion` and reports
`run.without_discussion: true` in review JSON. An older binary may reject the
flag; a successful child exit without that marker is recorded as
`discussion_isolation_unverified` and counted as a failed benchmark cell.
The harness preserves the child's actual exit code and raw output and never
retries without the flag. To deliberately benchmark an older binary, set
`without_discussion: false` and account for possible saved-session contamination.
Doctor and selected-case artifacts record the effective setting. Per-run
metrics, manifests, summaries, and comparisons distinguish
`requested_without_discussion` from `without_discussion_verified`; a missing
marker is never treated as proof of isolation. `benchmark compare` rechecks
the current review artifact against the original stdout SHA-256 recorded as
`review_json_sha256` in run metrics, summaries and the manifest. Missing digests
cannot verify isolated runs. Replacing or corrupting the artifact invalidates prior
verification and fails a cell that requested discussion-free execution.

Selector-only `benchmark select` already uses a caller-owned selection run with
no saved review sessions. It preserves this case setting in its recipe but does
not need the full-review flag.

Candidate `profile` must reference a configured profile. Candidate PR hosts must
match the candidate profile's Git host. For the current full-pipeline
`validate`, `doctor`, and `run` commands, candidates must declare explicit
selection `model` and `effort`. Reviewer candidates must declare reviewer
`agent_dirs` and one reviewer model selector: either exact
`stages.reviewers.model` or floor-based `stages.reviewers.model_tier`.
Reviewer `effort` is optional. When omitted, the benchmark does not pass a
`--reviewer-effort` override, so each selected agent keeps the effort resolved
from its catalog entry and the profile runtime's `max_effort` ceiling. When
present, reviewer `effort` is an explicit override.
Selector-only `benchmark select` still requires explicit
`stages.selection.model` and `stages.selection.effort`, but it allows the
reviewer stage to be omitted. `stages.selection.prompt` is optional, but when
set it must reference a readable non-empty file relative to the suite. The
`stages.reviewers.agent_dirs` field must be present for full-pipeline
benchmarks, but it may be `[]` to rely only on profile and repo-local agent
sources. Selector-only benchmarks pass `stages.reviewers.agent_dirs` through to
the selection catalog when configured, but do not require it. `max_agents` and
`max_concurrency` are optional; omit them or set them to `0` to use the
corresponding `cr review` default. Negative max values are invalid.

`stages.synthesis` is an optional reserved public recipe for the second
orchestrator phase. Current benchmark commands preserve it in doctor output,
suite summaries, comparison artifacts, and selector `recipe.json` snapshots,
but they do not execute it. If `stages.synthesis` is present, `model` and
`effort` must be explicit non-empty values and `prompt` must not be an
explicitly blank string. Current commands do not fail on missing or unreadable
`stages.synthesis.prompt` files because synthesis benchmarking is still future
work.

`effort` is the suite field for effort or reasoning-effort configuration. The
selected adapter decides how to apply or translate it. Model IDs are
provider-specific; use IDs accepted by the candidate profile's configured LLM
provider and adapter.

Relative `stages.reviewers.agent_dirs` are resolved from the suite file
directory. Benchmark summaries include resolved agent directory metadata. The
`dir_metadata_hash` field is metadata-only provenance based on relative path,
file size, and file mode. It does not hash prompt contents and is not a full
source reproducibility fingerprint. Prompt file summaries record resolved path
and content hash without inlining prompt contents.

## Commands

Validate a suite without running reviews:

```bash
cr benchmark validate .codereview/benchmarks/oss-model-cost-check.yml
```

Inspect selected candidates, cases, agent directories, result path readiness,
and the selected `cr` binary without running reviews:

```bash
cr benchmark doctor .codereview/benchmarks/oss-model-cost-check.yml
cr benchmark doctor .codereview/benchmarks/oss-model-cost-check.yml \
  --candidate claude-sonnet-medium \
  --case merged-security-pr \
  --json
```

Run the selected candidate x case matrix:

```bash
cr benchmark run .codereview/benchmarks/oss-model-cost-check.yml
cr benchmark run .codereview/benchmarks/oss-model-cost-check.yml \
  --candidate claude-sonnet-medium \
  --case merged-security-pr \
  --results-dir .cr-bench/results/debug-run \
  --json
```

Run the selected candidate x case matrix through the extracted selection phase
only:

```bash
cr benchmark select .codereview/benchmarks/oss-model-cost-check.yml
cr benchmark select .codereview/benchmarks/oss-model-cost-check.yml \
  --candidate claude-sonnet-medium \
  --case merged-security-pr \
  --results-dir .cr-bench/results/debug-select \
  --json
```

Compare an already-completed benchmark result directory:

```bash
cr benchmark compare .cr-bench/results/debug-run
cr benchmark compare .cr-bench/results/debug-run --json
```

Replay recorded report-identity decisions from one explicit local fixture:

```bash
cr benchmark replay .codereview/benchmarks/report-identity-example.json
```

`replay` emits one compact JSON result to stdout. It reads only the named fixture
and does not load a profile, model catalog, ledger, provider, or adapter. It does
not create result files or initiate model calls. The command returns 0 for
`complete` and `unresolved`, 1 for a valid result with recorded `error` choices
or for an I/O failure, and 2 for invalid arguments, flags, or fixture content.

The v1 fixture is one JSON object with these required top-level fields:

| Field | Values and shape |
|-------|------------------|
| `schema_version` | Integer `1`. |
| `task` | `cohort` or `prior_threads`. |
| `findings` | Required array of the normal finding fields `finding_id`, `severity`, `file_path`, `anchor`, and `body`; optional `anchoring` is preserved. `anchor` requires `kind` and may include `side` and `line`. |
| `threads` | Required array of `thread_id`, `eligible`, `file_path`, and `body`. `cohort` requires an empty array. |
| `decisions` | Required array of `left_id`, `right_id`, `choice`, and `input_sha256`. Choices are `same`, `different`, `abstain`, or `error`; each hash is 64 lowercase hexadecimal characters. |

Finding IDs and thread IDs use unique, case-sensitive identifiers of 1–128
ASCII bytes from `A-Z`, `a-z`, `0-9`, `.`, `_`, and `-`. IDs are unique across
both arrays. Severity is exactly `blocking`, `major`, `minor`, or `nits`;
`anchor.kind` is exactly `file` or `line`. A line anchor requires `side` of
`LEFT` or `RIGHT` and a positive `line`; a file anchor omits both. Optional
`anchoring` is `inline`, `file-level-native`, `file-level-fallback`, or
`rollup-only`. Paths and bodies are preserved exactly; whitespace-only values
are rejected. `eligible` must be a JSON boolean and is supplied by the caller,
with no thread-status or author inference.

For each expected pair, the hash is SHA-256 of compact UTF-8 JSON for the fixed
projection `{"schema_version":1,"task":"...","left":<record>,"right":<record>}`.
Finding keys follow `finding_id`, `severity`, `file_path`, `anchor`, `body`, and
optional `anchoring`; anchor keys follow `kind`, optional `side`, optional
`line`. Thread keys follow `thread_id`, `eligible`, `file_path`, `body`. Cohort
pairs use finding input order; prior-thread pairs use finding then thread order.
Hashes attest to this pair projection, not to fixture bytes, a model call, or
provenance. Cohort decisions may list their IDs in reverse order; replay
normalizes them to the pair's input order. Prior-thread decisions use finding
then thread order.

For example, an empty observation list is valid and leaves each expected pair
`missing`:

```json
{
  "schema_version": 1,
  "task": "cohort",
  "findings": [
    {"finding_id": "finding-a", "severity": "major", "file_path": "src/a.go", "anchor": {"kind": "file"}, "body": "Synthetic example finding A"},
    {"finding_id": "finding-b", "severity": "minor", "file_path": "src/b.go", "anchor": {"kind": "file"}, "body": "Synthetic example finding B"}
  ],
  "threads": [],
  "decisions": []
}
```

Every expected pair, including a missing pair, is returned with the hash of its
fixed v1 input projection. A caller can replay once to obtain those hashes, then
record decisions against that exact pair input. Fixture whitespace and decision
ordering do not change pair hashes or result ordering. V1 rejects unknown and
duplicate object keys, missing or null arrays, malformed IDs, invalid enums,
unknown pairs, duplicate pairs, and hash mismatches. The input is limited to 4
MiB, 100 findings, 100 threads, 10,000 decisions, 10,000 expected pairs, 16 KiB
per body, and 4 KiB per path.

For `cohort`, the command compares every unordered finding pair. A complete
result forms groups only when `same` is an equivalence relation. Any missing,
abstained, failed, or contradictory observation makes every group a singleton;
the result includes deterministic issue records. For `prior_threads`, it compares
each finding with every eligible thread and retains every `same` association,
including multiple associations. Ineligible threads remain in the originals but
never become pairs. Both tasks retain every input record in the result. The
rollup fields are a compatibility projection; they do not choose a review event
or an action.

Use repeatable `--candidate <id>` and `--case <id>` flags for benchmark
selection. Do not use ambiguous benchmark model-selection language. Models are
stage recipe fields, not suite selectors.

`run` shells out to `cr review` for each selected run. The generated command
always uses dry-run JSON review mode:

```text
cr --profile <candidate.profile> review <case.pr> --dry-run --json ...
```

When set on the candidate, `run` also passes:

| Candidate field | Review flag |
|-----------------|-------------|
| `stages.selection.model` | `--selection-model <model>` |
| `stages.selection.effort` | `--selection-effort <effort>` |
| `stages.selection.prompt` | `--selection-prompt <path>` |
| `stages.reviewers.model` | `--reviewer-model <model>` |
| `stages.reviewers.model_tier` | `--reviewer-model-tier <tier>` |
| `stages.reviewers.effort` | `--reviewer-effort <effort>` when present; omitted to inherit agent/profile resolution |
| `stages.reviewers.agent_dirs[]` | `--agents-dir <path>` |
| `max_agents` | `--max-agents <n>` |
| `max_concurrency` | `--max-concurrency <n>` |

When set on the case, `run` also passes:

| Case field | Review flag |
|------------|-------------|
| `review_base_sha` | `--review-base-sha <sha>` |
| `review_head_sha` | `--review-head-sha <sha>` |
| `without_discussion` | `--without-discussion` when effectively true; defaults to true for pinned cases |

Other unset fields are omitted. Benchmark candidate artifacts record reviewer
`effort_source` as `inherited` or `override`, so reports can distinguish the
two execution recipes even when the inherited effort is resolved later per
agent. Effort values are `low`, `medium`, `high`, `xhigh`, or `max`; validation
rejects levels unsupported by the candidate profile's runtime. `claude_cli`,
`codex_cli`, `openai_api`, and `pi_rpc` support the full range;
`anthropic_api` supports through `high`. Effective extended-effort support
depends on the selected model and, for CLI adapters, the installed CLI.

Apart from the per-case `without_discussion` setting, posting, retry, approval,
thread-resolution, session, and live-review flags are never taken from the suite.

`--cr-bin <path>` selects the binary used for child review runs. If omitted,
`run` uses the current `cr` binary. `doctor` reports the binary it would use.

`select` does not use `--cr-bin`. It reuses the real in-process selection phase
instead of spawning a child `cr review` command. Each selected run maps suite
recipes directly into `pipeline.SelectionOnly`:

| Candidate field | Selection request field |
|-----------------|-------------------------|
| `stages.selection.model` | `SelectionModelOverride` |
| `stages.selection.effort` | `SelectionEffortOverride` |
| `stages.selection.prompt` | file contents loaded into `SelectionPromptInstructions` |
| `stages.reviewers.agent_dirs[]` | `AgentDirs`, each resolved against the suite file directory |
| `review_base_sha` / `review_head_sha` | pinned review SHAs |

Selector-only benchmarks do not run reviewer agents, do not run synthesis, do
not write `review.json`, and do not create reviewer findings or rollup
artifacts.

Optional `stages.synthesis` metadata is preserved in summaries and selector
`recipe.json` artifacts, but it is not executed. Dedicated synthesis
benchmarking remains out of scope for the current selector benchmark work.

`compare` reads the benchmark-owned artifacts in an existing results directory
and writes `comparison.json` and `comparison.md`. It is local-only: it does not
invoke models, re-read live PR state, mutate Git provider state, or require
provider credentials. `run` writes the same comparison artifacts automatically
after the suite artifacts are written.

`replay` is an offline pure assembly command. It does not read benchmark suites,
configuration, profiles, credentials, catalogs, ledger state, or provider data;
callers supply the input records and recorded pair decisions explicitly.

### `cr benchmark infer-system-one`

Run explicit report-identity inference over one caller-supplied replay fixture through a System One choice endpoint:

```bash
cr benchmark infer-system-one ./identity-input.json \
  --model decision-models/baseten-mammoth-27b-latest \
  --max-calls 100 \
  --output-dir ./identity-run
```

Mammoth-1 is a Base Labs/Baseten model selection in this example. The command has no default model. Its defaults select Baseten's `https://inference.baseten.co/v1/systemone` endpoint, `api-key` authentication and the `BASETEN_API_KEY` environment variable. Use `--endpoint`, `--auth-scheme api-key|bearer` and `--api-key-env NAME` to select another compatible destination and credential source. The environment-variable flag takes a name, never a secret. The selected HTTPS endpoint receives the key and report text; URLs with user information, queries or fragments are rejected, and redirects are not followed.

For TypeSafe Jev, choose the required `--model` from the authenticated account's [`GET /v1/models` catalog](https://docs.typesafe.ai/api/list-models); the [System One API reference](https://docs.typesafe.ai/api/system-one) documents the endpoint and response metadata. This example assumes the account lists `jev-latest` and returns the exact resolved model `jev-1.14.0`. `--expected-model` is an optional local check for that resolved model; it is not sent in the request. Without it, the response model must exactly match `--model`.

Set `TYPESAFE_API_KEY` in the process environment through your existing secret manager. `--api-key-env` takes only the environment variable name, never the key value.

```bash
cr benchmark infer-system-one internal/cmd/benchmarkcmd/testdata/identity-smoke/cohort.json \
  --model jev-latest --expected-model jev-1.14.0 --max-calls 3 \
  --endpoint https://api.typesafe.ai/v1/systemone --auth-scheme bearer \
  --api-key-env TYPESAFE_API_KEY --output-dir ./jev-cohort-smoke

cr benchmark infer-system-one internal/cmd/benchmarkcmd/testdata/identity-smoke/prior-threads.json \
  --model jev-latest --expected-model jev-1.14.0 --max-calls 1 \
  --endpoint https://api.typesafe.ai/v1/systemone --auth-scheme bearer \
  --api-key-env TYPESAFE_API_KEY --output-dir ./jev-prior-threads-smoke
```

These shared fixtures contain synthetic same/different controls, not captured provider responses, and they make no model-quality claim. Together they cap the smoke at four calls: three unordered cohort pairs and one eligible prior-thread pair. No live TypeSafe request runs in the normal Go test suite. No service tier is sent unless `--service-tier` is supplied. Missing prices leave attempted-call costs unknown; they are not treated as zero.

The command uses a choice-only System One client, not a full API SDK. The provider-neutral report-identity projection supplies the versioned prompt, complete report state and same/different/abstain mapping. Other question types and live review stages are outside this command. It does not read reviewer profiles, catalogs or ledger state.

The input is one v1 `cohort` or `prior_threads` fixture with an empty `decisions` array. `cohort` sends every unordered finding pair; `prior_threads` sends every finding and eligible thread pair. Every original record is retained, including ineligible threads. Each pair receives one sequential request. `--max-calls` is required, accepts 0–10000, and must cover the complete pair set. Empty pair sets need no key and make no requests. The command does not shorten records, retry, resume or continue after a failed request.

`--expected-model` declares the exact returned model ID to validate and defaults to `--model`. Supply it explicitly when a provider resolves a requested alias to another known model ID. Both selected IDs are recorded; arbitrary returned IDs are rejected. A rolling alias does not identify immutable weights. `--service-tier` is an optional provider extension and is omitted from requests unless supplied.

All pairs are preflighted before any request. `--max-state-bytes` and `--max-request-bytes` set local compact-JSON bounds, defaulting to 6144 and 8192 bytes. These guards do not establish a provider's token context window. Explicit truncation or context-overflow signals stop the run. `--timeout` is per request, defaults to 30s, and accepts 1s–120s.

Prices are optional and have no model-specific defaults. Supply both `--input-price-per-million` and `--output-price-per-million` with caller-verified USD rates to calculate estimates. An estimate requires both rates and valid token usage. Without either, an attempted call's cost remains unknown; explicit zero rates produce known zero. The aggregate `estimated_cost_usd` is a known subtotal and is null when calls occurred but no estimate is available. `unknown_cost_calls` identifies incomplete accounting. A zero-call run has known zero cost. Estimates do not include cache discounts and are neither an invoice nor a monetary ceiling.

`--output-dir` is required and must name a new directory. It is created with mode 0700 and contains `fixture.json`, `replay.json`, `receipts.json` and `manifest.json` with mode 0600. The manifest publishes last. Artifacts contain complete original report text and must stay out of source control. Receipts include selected configuration, IDs, hashes, choices, probabilities, token usage, timing and nullable cost estimates; raw provider request/response bodies and credential values are not saved. Use `cr benchmark replay` on the saved fixture to reproduce composition without a provider call.

The compact stdout summary includes configuration, output paths, pair counts, usage and cost accounting. `complete` and `unresolved` exit 0; a failed request or output write exits 1. Invalid arguments/fixtures and valid-input preflight failures exit 2. Preflight failure publishes retained error artifacts when local output permits; invalid arguments or malformed fixtures create no output directory. The unshipped `infer-mammoth` name has no compatibility alias.

### `cr benchmark infer-openai-decisions`

Run the same report-identity inference and replay over a local fixture through OpenAI's Decisions choice API:

```bash
cr benchmark infer-openai-decisions ./identity-input.json \
  --model gpt-6-luna \
  --max-calls 100 \
  --output-dir ./openai-identity-run
```

The command has no default model or price. Its default endpoint is `https://api.openai.com/v1/decisions`, and it reads the API key from `OPENAI_API_KEY` and sends it with Bearer authentication. `--endpoint` selects another HTTPS endpoint and `--api-key-env` selects another environment variable name; the key itself is never a flag value or saved in artifacts. `--expected-model` is an optional local check for a provider-resolved model ID and is not sent in the request. The command does not expose authentication-scheme, service-tier or reasoning-effort flags.

The OpenAI client implements only one named string-choice question. It sends the exact compact projected report state as `input`, followed by the shared identity instructions and the ordered `a`, `b`, and `c` criteria. It validates the returned named choice, confidence and full probability distribution. Other Decisions question types and live review stages are outside the command. Both inference commands use the same fixture schema, prompt, pair ordering, validation, retained replay and artifact format; neither is a model matrix or a quality or performance claim. See the [Decisions API reference](https://developers.openai.com/api/reference/resources/decisions/methods/create) and [Decisions guide](https://developers.openai.com/api/docs/guides/decisions) for the upstream API contract.

The same `--max-calls`, `--max-state-bytes`, `--max-request-bytes`, `--timeout` and optional input/output price flags apply. `--max-state-bytes` limits the UTF-8 projected input bytes. Input and output token totals remain available when usage reports them, but cost is estimated only when both cached-input and cache-write counts are known zero. Missing, null or nonzero cache counts make the attempted call's cost unknown, even when base token counts and caller-supplied rates are present. No cache pricing is inferred.

The command uses the same complete preflight, lazy credential lookup, fail-stop behavior and private output directory rules described above. It retains ineligible prior threads in `fixture.json` without sending them as pairs. Empty pair sets need no key and have known zero cost. Provider refusal is recorded as a failed attempt with validated usage retained, not as an abstention.

## Artifacts

Full-review `run` writes benchmark-owned artifacts under the selected results
directory:

```text
.cr-bench/results/<suite-id>/<timestamp>/
  manifest.json
  summary.jsonl
  suite-summary.json
  report.md
  comparison.json
  comparison.md
  0001-c01-k01-<candidate-id>-<case-id>/
    review.json
    stderr.txt
    metrics.json
```

Selector-only `select` writes the same suite-level summary files, but each run
directory contains selector-specific artifacts:

```text
.cr-bench/results/<suite-id>/select/<timestamp>/
  manifest.json
  summary.jsonl
  suite-summary.json
  report.md
  comparison.json
  comparison.md
  0001-c01-k01-<candidate-id>-<case-id>/
    selection.json
    recipe.json
    stderr.txt
    metrics.json
```

Run IDs include the matrix index, candidate index, case index, candidate ID, and
case ID. The `cNN` segment is the candidate index, and the `kNN` segment is the
case index; `k` avoids reusing `c` for both candidate and case:

```text
0001-c01-k01-claude-sonnet-medium-merged-security-pr
```

Suite-level artifacts:

| Artifact | Contents |
|----------|----------|
| `manifest.json` | Suite ID/path/hash, timestamps, selected candidates/cases, run IDs, and artifact paths. |
| `summary.jsonl` | One compact JSON run summary per line. |
| `suite-summary.json` | Full benchmark summary including selected inputs, counts, run summaries, and artifact paths. |
| `report.md` | Compact human-readable run table. |
| `comparison.json` | Deterministic candidate x case comparison, failure classification, usage fields, artifact paths, and either selected reviewers or anchor placement metadata depending on benchmark mode. |
| `comparison.md` | Compact human-readable comparison report emphasizing per-case results before aggregate totals. |

Per-run artifacts:

| Artifact | Contents |
|----------|----------|
| `review.json` | Raw stdout from `cr review --dry-run --json`. |
| `stderr.txt` | Stderr from the child `cr review` process. |
| `metrics.json` | Benchmark run summary for that candidate/case execution, including provider usage when available. This is not a raw provider metrics file. |

Selector-only per-run artifacts:

| Artifact | Contents |
|----------|----------|
| `selection.json` | Raw selector structured-output bytes when a selector turn occurred. On selector failures before a valid decode, this preserves the last available provider JSON bytes when possible. |
| `recipe.json` | Candidate and case recipe snapshot for that run, including prompt provenance metadata without prompt bodies. |
| `stderr.txt` | Selector runtime or validation failure text when the selector run failed. Successful selector runs usually leave this empty. |
| `metrics.json` | Benchmark run summary for that candidate/case selector execution, including selected reviewers/files and provider usage when available. |

Benchmark artifacts are written with owner-only file permissions where the
operating system supports them. Directories are owner-only as well.

An explicit `--results-dir` is used as the exact output directory. Re-running
with the same directory overwrites benchmark-owned artifact files and leaves
unknown files in place.

## Metrics

The MVP measures rather than grades. Current benchmark summary artifacts include:

- suite ID, suite path, suite SHA-256 hash, start and completion timestamps;
- selected candidates and cases;
- resolved candidate agent directory metadata;
- run ID, candidate ID, case ID, and PR URL;
- requested pinned review base/head SHAs when a case sets them, plus expected
  baseline SHAs when provided;
- child review or selector run exit code and duration in milliseconds;
- retry count, currently `0` because benchmark candidate/case executions are
  not retried by the runner;
- coarse failure classification derived from local run facts and exit codes;
- selected candidate stage metadata, including optional reserved synthesis
  recipes when configured;
- finding count and severity counts parsed from dry-run review JSON when the
  benchmark mode is full-review;
- selected reviewers/files and thread-action counts when the benchmark mode is
  selector-only;
- provider-reported usage from child review or selector agent logs when available,
  including LLM call count, turns, tool activity, tokens, cost, and per-phase
  agent log summaries;
- CR-owned Pi `cr_diff` evidence counts when present, under `usage.pi_diff`:
  `succeeded`, `failed`, `not_invoked`, and `incomplete` records aggregated
  across the run's agent logs;
- warning strings when child review output cannot be parsed or selector runs
  fail after partial execution;
- benchmark artifact paths.

`review.json` is preserved so analysis tools can inspect the underlying dry-run
review output. Other local review artifacts referenced by that JSON may contain
more detail, depending on adapter and review behavior.

`selection.json` is preserved so analysis tools can inspect the selector output
or invalid selector payloads without rerunning the benchmark. Selector summaries
record selected reviewer IDs and files directly, so comparison output can show
selector choices per candidate and case without opening raw artifacts.

Treat these metric families as nullable unless the producing adapter or
artifact actually reports them. Generated reports render unavailable run-level
usage as `n/a` instead of `0` so missing provider telemetry is not confused with
real zero-token or zero-cost usage. In JSON artifacts, token and cost metric
objects include `available` to distinguish explicitly reported zero values from
missing telemetry.

| Metric family | Notes |
|---------------|-------|
| Input tokens | Provider or adapter reported prompt/input tokens, when present in child review agent logs. |
| Output tokens | Provider or adapter reported completion/output tokens, when present in child review agent logs. |
| Thinking/reasoning tokens | Only present when a provider exposes a separate count. |
| Cache read | Provider or adapter reported cache-read tokens, when present in child review agent logs. |
| Cache create | Provider or adapter reported cache-write/create tokens, when present in child review agent logs. |
| Cost | Provider- or adapter-reported cost when available. Review artifacts may also carry a clearly marked API-equivalent estimate and its dated pricing basis; this is not subscription spend or provider billing. |
| Pinned diff evidence | CR-owned `codereview-pi-tool-evidence tool=cr_diff` records only. `succeeded`, `failed`, `not_invoked`, and `incomplete` are counts per run; failed, not-invoked, or incomplete evidence is degraded tool execution and does not redefine the general benchmark quality grade. A missing `pi_diff` object means no qualifying evidence was observed, not zero successful calls. |
| Selected agents | Selector-only benchmarks record selected reviewer IDs and files directly in suite summaries, JSONL, and comparison artifacts. Full-review benchmarks still rely on review artifacts and logs for downstream selection analysis. |
| Observed SHAs | Record when available from review artifacts or downstream analysis. Expected SHAs in cases are comparison metadata. |
| Anchor metrics | Computed by `comparison.json` and `comparison.md` when cases define anchors. They are placement-only. |

Finding counts and severity counts are not quality scores. They are raw measures
for comparing review behavior across candidate configurations.

## Anchors

An anchor is optional case metadata describing an objective placement target:

```yaml
anchors:
  - id: missing-auth-check
    file: internal/api/users.go
    side: RIGHT
    lines: [42, 45]
```

Anchors use a file path, a diff side (`RIGHT` or `LEFT`), and a changed-line
range. Comparison artifacts use anchors to answer placement questions only:

- Did a finding attach to this file?
- Did it attach to the expected diff side?
- Did it attach within this changed-line range?

Placement labels are mechanical:

| Label | Meaning |
|-------|---------|
| `anchor_overlap_hit` | Exactly one finding overlaps the expected file, side, and line range. |
| `anchor_overlap_miss` | No finding overlaps the expected anchor. |
| `multiple_anchor_overlaps` | More than one finding overlaps the same expected anchor. |
| `unmatched_finding` | A finding does not overlap any expected anchor for that case. |

Anchors do not answer semantic questions:

- Was the finding correct?
- Was it important?
- Was it appropriate for the repository's review culture?
- Should it block the PR?

Do not turn anchor matches into pass/fail grading in the benchmark MVP.

Comparison artifacts preserve placement metadata such as finding IDs, file,
side, and line. They do not copy finding bodies, rollups, prompt contents, or
other raw LLM-generated text from `review.json`.

## Privacy

Share suite files only when the PR URLs, IDs, names, notes, and expected SHA
metadata are appropriate for the repository or organization context.

Do not commit generated `.cr-bench/` results by default. Generated results can
include private diffs, model output, stderr, local paths, profile names, model
or provider metadata, run artifact paths, and usage details.

Do not inline prompt contents into public benchmark summaries by default.
Benchmark summaries use provenance such as suite hashes, artifact paths, and
`dir_metadata_hash` for agent directories. If you need prompt-content
reproducibility, keep that evidence in a private artifact or source-controlled
agent pack that is safe for the audience.
