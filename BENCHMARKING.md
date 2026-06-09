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
        model: claude-sonnet-4-5
        effort: medium
        prompt: prompts/selection-v1.md
      reviewers:
        model: claude-sonnet-4-5
        effort: medium
        agent_dirs:
          - ../agents
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

Candidate `profile` must reference a configured profile. Candidate PR hosts must
match the candidate profile's Git host. For the current full-pipeline
`validate`, `doctor`, and `run` commands, candidates must declare exact
provider model IDs and effort values for both `stages.selection` and
`stages.reviewers`. Selector-only `benchmark select` still requires explicit
`stages.selection.model` and `stages.selection.effort`, but it allows the
reviewer stage to be omitted. `stages.selection.prompt` is optional, but when
set it must reference a readable non-empty file relative to the suite. The
`stages.reviewers.agent_dirs` field must be present for full-pipeline
benchmarks, but it may be `[]` to rely only on profile and repo-local agent
sources. Selector-only benchmarks pass `stages.reviewers.agent_dirs` through to
the selection catalog when configured, but do not require it. `max_agents` and
`max_concurrency` are optional; omit them or set them to `0` to use the
corresponding `cr review` default. Negative max values are invalid.

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
| `stages.reviewers.effort` | `--reviewer-effort <effort>` |
| `stages.reviewers.agent_dirs[]` | `--agents-dir <path>` |
| `max_agents` | `--max-agents <n>` |
| `max_concurrency` | `--max-concurrency <n>` |

When set on the case, `run` also passes:

| Case field | Review flag |
|------------|-------------|
| `review_base_sha` | `--review-base-sha <sha>` |
| `review_head_sha` | `--review-head-sha <sha>` |

Unset fields are omitted. Posting, retry, approval, thread-resolution, session,
and live-review flags are never taken from the suite.

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
| `stages.reviewers.agent_dirs[]` | `AgentDirs` |
| `review_base_sha` / `review_head_sha` | pinned review SHAs |

Selector-only benchmarks do not run reviewer agents, do not run synthesis, do
not write `review.json`, and do not create reviewer findings or rollup
artifacts.

`compare` reads the benchmark-owned artifacts in an existing results directory
and writes `comparison.json` and `comparison.md`. It is local-only: it does not
invoke models, re-read live PR state, mutate Git provider state, or require
provider credentials. `run` writes the same comparison artifacts automatically
after the suite artifacts are written.

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
- finding count and severity counts parsed from dry-run review JSON when the
  benchmark mode is full-review;
- selected reviewers/files and thread-action counts when the benchmark mode is
  selector-only;
- provider-reported usage from child review or selector agent logs when available,
  including LLM call count, turns, tool activity, tokens, cost, and per-phase
  agent log summaries;
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
| Cost | Provider or adapter reported cost only. Do not use baked-in benchmark price tables for v1. |
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
