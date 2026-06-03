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
the base profile plus optional model, effort, agent directory, max-agent, and
concurrency overrides. A suite is the container that combines candidates and
cases into a matrix.

Profiles remain the account and execution context. They provide Git host/auth,
reviewer identity, LLM provider/auth/adapter, configured agent sources, and
review policy. Candidate fields only adjust dry-run review runtime behavior.
Candidate `agent_dirs` are passed as additional trusted agent sources; they
follow normal `cr review --agents-dir` precedence after profile and repo-local
base-branch sources.

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
`cr benchmark run` does not create or update `.gitignore`; add the rule
manually when the repository should keep benchmark results private. A typical
repository ignore rule is:

```gitignore
.cr-bench/
```

The default `run` output path is:

```text
.cr-bench/results/<suite-id>/<timestamp>/
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

Prefer the canonical `agent_dirs` field for candidate agent directory lists. The
loader currently accepts the draft alias `agents_dir` for compatibility and does
not emit a deprecation warning, but new suites should use `agent_dirs`. A
candidate cannot set both names.

```yaml
suite:
  id: oss-model-cost-check
  name: OSS model cost check
  version: 1

candidates:
  - id: claude-sonnet-medium
    profile: work-anthropic
    model: claude-sonnet-4-5
    effort: medium
    agent_dirs:
      - ../agents
    max_agents: 5
    max_concurrency: 5

  - id: kimi-low
    profile: work-fireworks
    model: moonshotai/Kimi-K2
    effort: low
    agent_dirs:
      - ../agents
    max_agents: 5

cases:
  - id: merged-security-pr
    pr: https://github.com/OWNER/REPO/pull/123
    expected_base_sha: abc1234
    expected_head_sha: def5678
    anchors:
      - id: missing-auth-check
        file: internal/api/users.go
        side: RIGHT
        lines: [42, 45]
```

IDs must be unique within their list and use letters, numbers, underscores, or
hyphens. PRs must be GitHub pull request URLs. Expected SHA fields are optional,
but when present they must be non-empty 7 to 64 character hexadecimal SHAs.

Candidate `profile` must reference a configured profile. Candidate PR hosts must
match the candidate profile's Git host. `model`, `effort`, `agent_dirs`,
`max_agents`, and `max_concurrency` are optional. Omit max fields or set them to
`0` to use the corresponding `cr review` default. Negative max values are
invalid.

`effort` is the suite field for effort or reasoning-effort configuration. The
selected adapter decides how to apply or translate it. Model IDs are
provider-specific; use IDs accepted by the candidate profile's configured LLM
provider and adapter.

Relative `agent_dirs` are resolved from the suite file directory. Benchmark
summaries include resolved agent directory metadata. The
`dir_metadata_hash` field is metadata-only provenance based on relative path,
file size, and file mode. It does not hash prompt contents and is not a full
source reproducibility fingerprint.

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

Use repeatable `--candidate <id>` and `--case <id>` flags for benchmark
selection. Do not use ambiguous benchmark model-selection language. Models are
candidate fields, not suite selectors.

`run` shells out to `cr review` for each selected run. The generated command
always uses dry-run JSON review mode:

```text
cr --profile <candidate.profile> review <case.pr> --dry-run --json ...
```

When set on the candidate, `run` also passes:

| Candidate field | Review flag |
|-----------------|-------------|
| `model` | `--llm-model <model>` |
| `effort` | `--llm-effort <effort>` |
| `agent_dirs[]` | `--agents-dir <path>` |
| `max_agents` | `--max-agents <n>` |
| `max_concurrency` | `--max-concurrency <n>` |

Unset fields are omitted. Posting, retry, approval, thread-resolution, session,
and live-review flags are never taken from the suite.

`--cr-bin <path>` selects the binary used for child review runs. If omitted,
`run` uses the current `cr` binary. `doctor` reports the binary it would use.

## Artifacts

Each run writes benchmark-owned artifacts under the selected results directory:

```text
.cr-bench/results/<suite-id>/<timestamp>/
  manifest.json
  summary.jsonl
  suite-summary.json
  report.md
  0001-c01-k01-<candidate-id>-<case-id>/
    review.json
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

Per-run artifacts:

| Artifact | Contents |
|----------|----------|
| `review.json` | Raw stdout from `cr review --dry-run --json`. |
| `stderr.txt` | Stderr from the child `cr review` process. |
| `metrics.json` | Benchmark run summary for that candidate/case execution, including provider usage when available. |

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
- child review exit code and duration in milliseconds;
- finding count and severity counts parsed from dry-run review JSON;
- provider-reported usage from child review agent logs when available,
  including LLM call count, turns, tool activity, tokens, cost, and per-phase
  agent log summaries;
- warning strings when child review output cannot be parsed;
- benchmark artifact paths.

`review.json` is preserved so analysis tools can inspect the underlying dry-run
review output. Other local review artifacts referenced by that JSON may contain
more detail, depending on adapter and review behavior.

Treat these metric families as nullable unless the producing adapter or
artifact actually reports them:

| Metric family | Notes |
|---------------|-------|
| Input tokens | Provider or adapter reported prompt/input tokens, when present in child review agent logs. |
| Output tokens | Provider or adapter reported completion/output tokens, when present in child review agent logs. |
| Thinking/reasoning tokens | Only present when a provider exposes a separate count. |
| Cache read | Provider or adapter reported cache-read tokens, when present in child review agent logs. |
| Cache create | Provider or adapter reported cache-write/create tokens, when present in child review agent logs. |
| Cost | Provider or adapter reported cost only. Do not use baked-in benchmark price tables for v1. |
| Selected agents | Use raw review artifacts or agent logs when available. The benchmark summary records selected candidate inputs, resolved agent directories, and usage phase names, not a stable selected-agent table today. |
| Observed SHAs | Record when available from review artifacts or downstream analysis. Expected SHAs in cases are comparison metadata. |
| Anchor metrics | Not computed by the current runner. If added later, they should remain placement-only. |

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
range. The current suite validator accepts and validates anchor metadata. The
runner does not compute anchor hit/miss metrics today.

If anchor hit/miss reporting is added later, it should answer placement
questions only:

- Did a finding attach to this file?
- Did it attach to the expected diff side?
- Did it attach within this changed-line range?

Anchors do not answer semantic questions:

- Was the finding correct?
- Was it important?
- Was it appropriate for the repository's review culture?
- Should it block the PR?

Do not turn anchor matches into pass/fail grading in the benchmark MVP.

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
