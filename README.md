# codereview-cli

`cr` is the Open CLI Collective code-review CLI. It runs automated pull-request
reviews, records review state locally, posts review output back to GitHub, and
keeps enough durable metadata to resume, repair, inspect, and prune runs.

## Not GitHub Code Review

This project is not affiliated with GitHub and is not a replacement for human
review. It is a CLI for orchestrating LLM-backed review agents against GitHub
pull requests.

Use `cr` when you want to:

- preview review actions before posting anything;
- run a live PR review with idempotent posting and resume behavior;
- reuse named LLM sessions across related live reviews;
- inspect trusted reviewer agents available to a repository;
- manage local review run data and credentials from the terminal.

## Installation

Package-manager installs are available for versions after their
package-channel release jobs publish successfully.

### Homebrew

```bash
brew install --cask open-cli-collective/tap/codereview-cli
```

### Windows Package Managers

```powershell
winget install OpenCLICollective.codereview-cli
choco install codereview-cli
```

### Linux Packages

`cr` release builds include `.deb` and `.rpm` packages for the
[open-cli-collective/linux-packages](https://github.com/open-cli-collective/linux-packages)
repository.

### Binary Download

Download a release archive from the
[Releases page](https://github.com/open-cli-collective/codereview-cli/releases).

### From Source

```bash
go install github.com/open-cli-collective/codereview-cli/cmd/cr@latest
```

### Manual Build

```bash
git clone https://github.com/open-cli-collective/codereview-cli.git
cd codereview-cli
make build
```

## Platform Support

`cr` stores secrets in the OS credential store through the shared
`cli-common/credstore` library.

| Platform | Default credential backend |
|----------|----------------------------|
| macOS | Keychain |
| Windows | Credential Manager |
| Linux | Secret Service |

Supported backend names are `keychain`, `wincred`, `secret-service`, `file`,
`pass`, and `memory`. Backend selection precedence is:

1. `--backend <name>`
2. `CODEREVIEW_KEYRING_BACKEND=<name>`
3. `keyring.backend` in `config.yml`
4. OS default

Secrets are never written to `config.yml`. Non-secret config lives in the
`codereview` config directory resolved by the operating system, and durable
review data lives under the OS data directory for the `cr` binary.

## Authentication And Setup

`cr` v1 supports GitHub personal access token authentication for Git-host
operations. The credential key for GitHub PATs is `git_token`.

Quick setup with adapter-managed LLM credentials:

```bash
cr init --non-interactive \
  --git-token-from-env GITHUB_TOKEN
```

Setup with Pi's local RPC runtime. Install Pi's coding agent and make sure the
`pi` binary is available on `PATH` before running `cr review`. New installs
should use the current npm package (`@earendil-works/pi-coding-agent`); existing
installs from the previous npm scope can also work if their `pi` binary supports
the required `--mode rpc` and `--system-prompt` flags.

```bash
cr init --non-interactive \
  --llm-provider pi \
  --llm-auth subscription \
  --llm-adapter pi_rpc \
  --git-token-from-env GITHUB_TOKEN
```

Setup with a direct LLM API key:

```bash
cr init --non-interactive \
  --llm-auth api_key \
  --llm-adapter anthropic_api \
  --llm-api-key-from-env ANTHROPIC_API_KEY \
  --git-token-from-env GITHUB_TOKEN
```

Setup a named profile:

```bash
cr --profile work init --non-interactive \
  --git-token-from-env GITHUB_TOKEN \
  --agent-source ~/.config/codereview/agents
```

Add or replace one credential later:

```bash
printf '%s' "$GITHUB_TOKEN" | cr set-credential \
  --ref codereview/default \
  --key git_token \
  --stdin \
  --overwrite
```

Credential refs use the `codereview/<profile>` service/profile form. `cr`
accepts secrets by `--stdin` or `--from-env` during setup and credential writes;
it does not read runtime tokens directly from arbitrary environment variables.
Reviewer credentials use a separate credential ref that also stores key
`git_token`; keep it distinct from the user Git ref.

### Org Deployment / MDM

For managed rollouts, ship non-secret config and pre-stage secrets in the
target user's credential store. The staging commands must run as the target OS
user who will run `cr`, not as root, SYSTEM, or an administrator account whose
keyring is different from the target user's keyring.

`reviewer_credentials` is PAT-only in v1. It posts through the GitHub account
that owns the staged reviewer PAT, so a shared reviewer or bot PAT is an access
secret even when it is distributed org-wide. Pre-stage it into each target
user's credential store, rotate or revoke it with the same
`set-credential --overwrite` flow, and do not store it in config files, agent
sources, installers, logs, or shell profiles. GitHub App reviewer credentials
are tracked in [issue #76](https://github.com/open-cli-collective/codereview-cli/issues/76)
for org-controlled reviewer identity and installation-scoped auth.

Either omit `keyring.backend` to use the platform default, or set it
per-platform and run `set-credential` with the same backend selection.

Example non-secret config template:

```yaml
default_profile: work
profiles:
  work:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/work
    reviewer_credentials:
      auth_mode: pat
      credential_ref: codereview/work-reviewer
    llm:
      provider: anthropic
      auth: api_key
      adapter: anthropic_api
      credential_ref: codereview/work-llm
    agent_sources:
      - ~/.config/codereview/agents
    review_policy:
      major_event: comment
```

For adapter-managed LLM credentials, use `auth: subscription` and omit
`llm.credential_ref`. Pi-backed profiles must set `provider: pi`,
`auth: subscription`, and `adapter: pi_rpc` together:

```yaml
llm:
  provider: pi
  auth: subscription
  adapter: pi_rpc
```

Agent definitions are non-secret deployment material, not credentials. Ship
them separately from keyring pre-staging and point `agent_sources` at the
managed directory. Recommended managed paths:

| Platform | Example managed agent source |
|----------|------------------------------|
| macOS | `/Library/Application Support/codereview/agents` |
| Windows | `C:\ProgramData\codereview\agents` |
| Linux | `/etc/codereview/agents` |

Use the standard agent layout under that directory:

```text
agents/
  security/
    index.yaml
    secrets/
      index.yaml
      prompt.md
```

`cr config show --json` reports each configured source by path, presence,
status, canonical path, warnings, and SHA-256 fingerprint prefix without
inlining `index.yaml` or `prompt.md`. Missing or unreadable sources are shown as
deployment status in `config show`; commands that actually load agents, such as
`cr agents` and `cr review`, fail fast until the source is fixed.

Avoid profile sources under temp directories, relative paths, or Git worktrees.
Those locations are warned because they are easy for local scripts or PR authors
to mutate. Symlinked sources are accepted, but `cr` records the canonical path
and fingerprints the resolved files.

Pre-stage each secret with `set-credential`:

```bash
printf '%s' "$USER_GITHUB_TOKEN" | cr set-credential \
  --ref codereview/work \
  --key git_token \
  --stdin \
  --overwrite

printf '%s' "$REVIEW_BOT_GITHUB_TOKEN" | cr set-credential \
  --ref codereview/work-reviewer \
  --key git_token \
  --stdin \
  --overwrite

printf '%s' "$ANTHROPIC_API_KEY" | cr set-credential \
  --ref codereview/work-llm \
  --key anthropic_api_key \
  --stdin \
  --overwrite
```

Then verify the deployed profile without running `init`:

```bash
cr config show --json
cr me --all --json
```

Do not run `cr init` after pre-staging; the profile and credentials are already
deployed.

Environment variables in the examples above are setup ingress only. Runtime
commands resolve service credentials from `config.yml` and the configured
credential backend.

## Configuration

Run `cr config show` to inspect the active profile and credential status.

```yaml
default_profile: default
keyring:
  backend: keychain
profiles:
  default:
    git:
      host: github.com
      auth_mode: pat
      credential_ref: codereview/default
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
    agent_sources:
      - ~/.config/codereview/agents
    review_policy:
      major_event: comment
      allow_self_approve: false
      resolve_threads: auto
      resolve_after: 24h
data:
  retention:
    max_age_days: 90
    enforcement: at_write
```

Supported values:

| Field | Values |
|-------|--------|
| `git.auth_mode` | `pat` is implemented in v1. `oauth_device` and `github_app` are recognized by the config schema but not implemented; validation rejects them in v1. GitHub App support is tracked in [issue #76](https://github.com/open-cli-collective/codereview-cli/issues/76). |
| `llm.provider` | `anthropic`, `openai`, `pi` |
| `llm.auth` | `subscription`, `api_key` |
| `llm.adapter` | `claude_cli`, `anthropic_api`, `openai_api`, `pi_rpc` are usable for review. `codex_cli` is recognized by the config schema but rejected by `cr review` until no-tools mode is explicit. |
| `review_policy.major_event` | `comment`, `request_changes` |
| `review_policy.resolve_threads` | `auto`, `never` |
| `data.retention.enforcement` | `at_write` applies review-time pruning before each `cr review`; `manual_only` disables review-time pruning and leaves `cr data prune` as the explicit maintenance path. |

`subscription` LLM auth means the adapter owns its own credentials, such as a
logged-in CLI or local runtime. `api_key` LLM auth requires
`llm.credential_ref` and stores a provider-specific API key in the credential
backend.

For Anthropic subscription profiles, `adapter: claude_cli` runs Claude Code
background jobs, reads the review result from an adapter-owned scratch file, and
records the Claude provider session returned by the job state.

Credential key matrix:

| Profile field | Purpose | Auth/provider | Required keys | Optional keys | v1 behavior |
|---------------|---------|---------------|---------------|---------------|-------------|
| `git.credential_ref` | User Git host auth | `pat` | `git_token` | None | Supported |
| `reviewer_credentials.credential_ref` | Reviewer Git host auth | `pat` | `git_token` | None | Supported; must use a distinct ref from `git.credential_ref` in the same profile |
| `git.credential_ref` / `reviewer_credentials.credential_ref` | Git host auth | `github_app` | None | None | Reserved for [issue #76](https://github.com/open-cli-collective/codereview-cli/issues/76); config recognizes the mode but v1 rejects it and does not accept future keys such as `git_app_private_key` |
| `git.credential_ref` / `reviewer_credentials.credential_ref` | Git host auth | `oauth_device` | None | None | Reserved; config recognizes the mode but v1 rejects it and does not accept future keys such as `git_oauth_access_token` or `git_oauth_refresh_token` |
| `llm.credential_ref` | Anthropic direct API auth | `api_key` + `anthropic` | `anthropic_api_key` | None | Supported |
| `llm.credential_ref` | OpenAI direct API auth | `api_key` + `openai` | `openai_api_key` | None | Supported |
| Omitted `llm.credential_ref` | Adapter-managed LLM auth | `subscription` + `anthropic`/`pi` | None | None | Supported; credentials are owned by the selected adapter |
| `llm.credential_ref` | Pi direct API auth | `api_key` + `pi` | None | None | Unsupported; use adapter-managed `subscription` auth with `pi_rpc` |

Upgrade note: pre-matrix versions used the generic `llm_api_key` key for direct
LLM API credentials. Re-provision API-key LLM refs with `anthropic_api_key` or
`openai_api_key`; `llm_api_key` is not accepted by v1 `set-credential`.

## Common Workflows

Preview a review without posting:

```bash
cr review --dry-run https://github.com/OWNER/REPO/pull/123
```

Run a live review:

```bash
cr review https://github.com/OWNER/REPO/pull/123
```

Run a live review and fail the command when a major or blocking finding exists:

```bash
cr review --fail-on major https://github.com/OWNER/REPO/pull/123
```

Force a fresh live review instead of resuming or exiting from prior markers:

```bash
cr review --rerun https://github.com/OWNER/REPO/pull/123
```

Retry missing or failed required posts without rerunning the LLM review:

```bash
cr review --retry-posts https://github.com/OWNER/REPO/pull/123
```

Reuse a named LLM session for a series of related live reviews:

```bash
cr review --session release-train https://github.com/OWNER/REPO/pull/123
cr sessions show release-train
```

Inspect trusted agents for a PR:

```bash
cr agents list https://github.com/OWNER/REPO/pull/123
cr agents show harness:reviewer https://github.com/OWNER/REPO/pull/123
```

Inspect and clean local data:

```bash
cr data show
cr data prune --dry-run
cr data prune --keep-last 10
cr data purge --dry-run
```

Validate and run a benchmark suite:

```bash
cr benchmark validate .codereview/benchmarks/reviewer.yml
cr benchmark doctor .codereview/benchmarks/reviewer.yml --json
cr benchmark run .codereview/benchmarks/reviewer.yml \
  --candidate claude-sonnet-medium \
  --case merged-security-pr
```

See [BENCHMARKING.md](BENCHMARKING.md) for suite schema, terminology, artifact
layout, privacy guidance, and metric caveats.

## Command Reference

All commands accept the global flags:

| Flag | Semantics |
|------|-----------|
| `--profile <name>` | Select a configured profile. Empty means `default_profile` from config, or `default` during `init` before config exists. |
| `--backend <name>` | Select the credential backend for this invocation. One of `keychain`, `wincred`, `secret-service`, `file`, `pass`, `memory`. |

### `cr`

```text
cr [flags]
cr [command]
```

With no subcommand, `cr` prints help. `cr --version` prints the same version
line as `cr version`. Unknown commands and malformed flags return usage errors.

### `cr version`

```text
cr version
```

Prints the build version as `cr <version-info>`. It takes no arguments.

### `cr init`

```text
cr init [flags]
```

Creates or updates non-secret config. In v1, `--non-interactive` is required.
If the selected profile already exists, pass `--replace-profile` to replace the
profile config.

Flags:

| Flag | Semantics |
|------|-----------|
| `--non-interactive` | Required in v1. Run without prompts. |
| `--git-host <host>` | Git host, default `github.com`. The PR host must match this value. |
| `--git-credential-ref <ref>` | Credential ref for Git auth. Defaults to `codereview/<profile>`. |
| `--git-token-stdin` | Read the Git token from stdin and write key `git_token`. |
| `--git-token-from-env <env>` | Read the Git token from an environment variable and write key `git_token`. |
| `--reviewer-credential-ref <ref>` | Credential ref for reviewer Git auth. Defaults to `codereview/<profile>-reviewer` when reviewer credentials are requested. |
| `--reviewer-auth-mode <mode>` | Reviewer Git auth mode, default `pat`. Reserved modes are recognized by config but not implemented in v1. |
| `--reviewer-token-stdin` | Read the reviewer Git token from stdin and write key `git_token`. |
| `--reviewer-token-from-env <env>` | Read the reviewer Git token from an environment variable and write key `git_token`. |
| `--llm-provider <provider>` | LLM provider, default `anthropic`; also selects whether API-key ingress writes `anthropic_api_key` or `openai_api_key`. `pi` is adapter-managed and does not accept API-key ingress. |
| `--llm-auth <mode>` | LLM auth mode, default `subscription`. Use `api_key` for keyring-managed direct API keys. |
| `--llm-adapter <adapter>` | LLM adapter, default `claude_cli`. |
| `--llm-credential-ref <ref>` | Credential ref for LLM API-key auth. Defaults to `codereview/<profile>-llm` when `--llm-auth api_key`. |
| `--llm-api-key-stdin` | Read the LLM API key from stdin and write `anthropic_api_key` or `openai_api_key` according to `--llm-provider`. |
| `--llm-api-key-from-env <env>` | Read the LLM API key from an environment variable and write `anthropic_api_key` or `openai_api_key` according to `--llm-provider`. |
| `--agent-source <path>` | Add a trusted agent source directory. Repeatable. |
| `--major-event <policy>` | `comment` or `request_changes`. Controls review event for major findings. |
| `--allow-self-approve` | Store profile policy allowing self approval. Live review can still require `--allow-self-approve` depending on invocation. |
| `--resolve-threads <policy>` | `auto` or `never`. Empty leaves thread resolution unset. |
| `--resolve-after <duration>` | Store a validated duration such as `24h` for future thread-resolution policy. Current review planning uses `resolve_threads`/`--no-resolve-threads`, not this delay. |
| `--overwrite` | Replace existing keyring entries written by this command. |
| `--replace-profile` | Replace an existing profile config. |

Only one stdin secret ingress flag may be used at a time. Reviewer credentials
use key `git_token` under their own credential ref, so
`--reviewer-credential-ref` must differ from `--git-credential-ref`. LLM
API-key ingress requires `--llm-auth api_key`. `--overwrite` with API-key auth
requires an LLM key ingress flag. `--allow-self-review` is intentionally
runtime-only on `cr review`; `init` only stores the profile-level self-approval
policy.

### `cr set-credential`

```text
cr set-credential --ref <ref> --key <key> (--stdin | --from-env <env>) [flags]
```

Writes one secret value to the credential store. Globally allowed keys are
`git_token`, `anthropic_api_key`, and `openai_api_key`. When `config.yml`
declares the target ref, `set-credential` narrows that global allowlist to the
exact key set expected for that ref. User Git refs and reviewer refs both use
`git_token`; Anthropic LLM API-key refs use `anthropic_api_key`; OpenAI LLM
API-key refs use `openai_api_key`.

Flags:

| Flag | Semantics |
|------|-----------|
| `--ref <ref>` | Required credential ref, such as `codereview/default`. |
| `--key <key>` | Required key name: `git_token`, `anthropic_api_key`, or `openai_api_key`. |
| `--stdin` | Read the secret from stdin. |
| `--from-env <env>` | Read the secret from an environment variable. |
| `--overwrite` | Replace an existing key. Without it, existing keys are not overwritten. |
| `--json` | Emit a JSON result. |

Exactly one of `--stdin` or `--from-env` is required.

### `cr config show`

```text
cr config show [--json]
```

Shows the resolved active profile, selected credential backend and source,
credential refs, non-secret profile config, review policy, and data retention.
For each declared credential ref, it reports whether expected keys are present.
For each configured agent source, it reports deployment-material status,
canonical path, warnings, and SHA-256 fingerprint prefix without inlining agent
definition contents.

`--json` emits the same information as structured JSON.

### `cr config clear`

```text
cr config clear [--all] [--dry-run] [--json]
```

Deletes stored credentials declared by the active profile.

Flags:

| Flag | Semantics |
|------|-----------|
| `--all` | Also remove the active profile from `config.yml` and clear the disposable cache root. Inactive profiles and durable data are not touched. |
| `--dry-run` | Report the credential refs, config profile reset, and cache cleanup that would happen without deleting credentials, config, cache, or data. |
| `--json` | Emit a JSON result. |

Without `--all`, this removes secret keyring entries only and leaves
`config.yml` intact. With `--all`, `--profile <name>` selects the profile to
reset; otherwise the default profile is reset. If other profiles remain after a
profile reset, the default profile is updated deterministically to the first
remaining profile name when needed. If the last profile is reset, `config.yml`
is removed.

`config clear` never touches durable review data. Use `cr data purge` for the
data pillar.

### `cr me`

```text
cr me [--all] [--json]
```

Resolves the active git-host identity using configured posting credentials and
caches the identity in config. With `--all`, refreshes every configured profile
and verifies both user Git and reviewer identity credentials when a profile
declares both. `--json` emits structured output. `--all` cannot be combined
with `--profile`.

### `cr agents list`

```text
cr agents list [PR] [--agents-dir <path> ...] [--json]
```

Lists trusted review agents. If a PR URL is supplied, repo-local agents are
loaded from the PR base branch under `.codereview/agents`, not from the PR head.
This keeps unreviewed agent changes from affecting their own review.
Filesystem profile and flag sources include structured provenance with
configured path, canonical path, warnings, and SHA-256 fingerprint prefix so an
operator can verify which org-shipped agent version was used.

Agent source precedence is profile sources, repo-local base-branch agents, then
`--agents-dir` sources. Later sources override earlier agents with the same ID.

Flags:

| Flag | Semantics |
|------|-----------|
| `--agents-dir <path>` | Additional trusted agents directory. Repeatable. |
| `--json` | Emit JSON. |

### `cr agents show`

```text
cr agents show <name> [PR] [--agents-dir <path> ...] [--json]
```

Shows one agent, including category metadata, model, effort, file globs,
`applies_when`, `needs_full_file_content`, prompt, provenance, and trust note
when repo-local agents are considered.

### `cr review`

```text
cr review <PR> [flags]
```

Runs the automated review pipeline for a GitHub pull request URL. The PR host
must match the active profile's `git.host`.

Modes:

| Flag | Semantics |
|------|-----------|
| `--dry-run` | Plan review actions, write local artifacts, and print the plan without posting. |
| `--no-post` | Alias for `--dry-run`. |
| `--rerun` | Bypass gate resume/early-exit behavior and start a fresh live review. Mutually exclusive with `--retry-posts`. |
| `--retry-posts` | Retry missing or failed required posts for an existing run without rerunning LLM planning. Mutually exclusive with `--rerun` and incompatible with `--session`. |

Review selection and execution flags:

| Flag | Semantics |
|------|-----------|
| `--agents-dir <path>` | Additional trusted agents directory. Repeatable. |
| `--max-agents <n>` | Limit selected reviewer agents. Omit the flag or pass `0` for the default limit of 5. Negative values are rejected. |
| `--max-concurrency <n>` | Limit concurrent reviewer agents. Omit the flag or pass `0` for the default limit of 5. Negative values are rejected. |
| `--llm-model <model>` | Override the LLM model for dry-run review. Requires `--dry-run` or `--no-post`; without either flag the command returns usage error exit code 2 before running review. |
| `--llm-effort <effort>` | Override the LLM effort for dry-run review. Requires `--dry-run` or `--no-post`; without either flag the command returns usage error exit code 2 before running review. |
| `--review-base-sha <sha>` | Review this base commit SHA instead of the PR's current base SHA. Requires `--review-head-sha` and `--dry-run` or `--no-post`. |
| `--review-head-sha <sha>` | Review this head commit SHA instead of the PR's current head SHA. Requires `--review-base-sha` and `--dry-run` or `--no-post`. |
| `--session <name>` | Reuse a named LLM session for live reviews. Not allowed with `--dry-run`, `--no-post`, or `--retry-posts`. |
| `--verbose` | Include nits in review output and emit additional diagnostics. |

Policy and output flags:

| Flag | Semantics |
|------|-----------|
| `--fail-on <severity>` | Exit 1 if any finding is at or above `blocking`, `major`, `minor`, or `nits`. |
| `--allow-self-review` | Allow reviewer credentials that resolve to the PR author. |
| `--allow-self-approve` | Allow approval when the posting identity is the PR author. |
| `--no-resolve-threads` | Do not plan thread-resolution actions. Also implied by profile `resolve_threads: never`. |
| `--json` | Emit JSON. |

Live review uses a gate before planning or posting. If matching complete markers
already exist for the current head/base/profile/posting identity, `cr review`
exits early. If a prior compatible partial run exists, it resumes or repairs
posting from durable local state. If the PR base moved, live review aborts with
an upstream exit. `--rerun` starts fresh. `--retry-posts` replays required posts
that are missing or failed without rerunning the LLM review.

Dry-run output contains planned actions and artifact paths. Dry-run action
markers are reported as omitted because nothing is posted.

Live text output includes run ID, gate status, decision, outcome, PR, artifacts,
and post counts. JSON output includes `run`, `status`, `decision`, `message`,
`outbox`, `artifacts`, and `fail_on_triggered`.

### `cr benchmark`

```text
cr benchmark <command>
```

Validates, inspects, runs, and compares benchmark suites. See
[BENCHMARKING.md](BENCHMARKING.md) for the full benchmark guide.

### `cr benchmark validate`

```text
cr benchmark validate <suite.yml>
```

Validates benchmark suite schema and configured profile compatibility without
running reviews.

### `cr benchmark doctor`

```text
cr benchmark doctor <suite.yml> [--candidate <id> ...] [--case <id> ...] [--results-dir <path>] [--cr-bin <path>] [--json]
```

Inspects benchmark readiness without running reviews. It reports the selected
candidates and cases, resolved results directory, selected `cr` binary, profile
availability, model/effort fields, agent directories, and warnings.

### `cr benchmark run`

```text
cr benchmark run <suite.yml> [--candidate <id> ...] [--case <id> ...] [--results-dir <path>] [--cr-bin <path>] [--json]
```

Runs the selected candidate x case matrix by invoking `cr review --dry-run
--json` for each run. It always uses dry-run review mode, captures stdout and
stderr separately, records each child exit code, and writes generated artifacts
under `.cr-bench/results/<suite-id>/<timestamp>/` unless `--results-dir` is set.

Use `--candidate` and `--case` for benchmark selection. Candidate YAML fields
such as `model`, `effort`, `agent_dirs`, `max_agents`, and `max_concurrency`
control review runtime overrides. Case YAML fields `review_base_sha` and
`review_head_sha` pin the exact base/head commit pair reviewed by the dry-run
child command.

### `cr benchmark compare`

```text
cr benchmark compare <results-dir> [--json]
```

Reads an existing benchmark results directory and writes deterministic
`comparison.json` and `comparison.md` artifacts. Comparison is local-only: it
does not invoke models, read live PR state, mutate Git provider state, or
require provider credentials. `cr benchmark run` writes the same comparison
artifacts automatically.

### `cr sessions list`

```text
cr sessions list [--json]
```

Lists named LLM sessions in name order. Text output shows name, profile,
provider, adapter, model, host, and last-used time. JSON output includes the
provider session ID plus created and last-used timestamps.

### `cr sessions show`

```text
cr sessions show <name> [--json]
```

Shows one named LLM session. Missing sessions return an error. Text output
includes the provider session ID.

### `cr sessions delete`

```text
cr sessions delete <name> [--json]
```

Deletes one named LLM session row. It does not delete provider-side session
state. Missing sessions return an error.

### `cr data show`

```text
cr data show [--json]
```

Shows local durable data usage: data root, ledger path, runs root, run counts,
live/dry-run counts, outcome counts in JSON, oldest/newest run timestamps,
artifact bytes, and orphan artifact counts/bytes.

### `cr data prune`

```text
cr data prune [--older-than <duration> | --keep-last <n>] [--dry-run] [--json]
```

Prunes selected ledger runs and their artifacts, then removes orphan artifact
directories. The default with no selector applies built-in manual retention:
live runs older than 90 days and dry-run runs older than 7 days. This manual
default is independent from `data.retention.*` review-time pruning config.

Flags:

| Flag | Semantics |
|------|-----------|
| `--older-than <duration>` | Prune runs older than the given positive duration. Mutually exclusive with `--keep-last`. |
| `--keep-last <n>` | Keep the newest non-negative `n` runs per post mode and prune the rest. Mutually exclusive with `--older-than`. |
| `--dry-run` | Report selected runs and orphans without deleting. |
| `--json` | Emit JSON including selected/deleted runs, orphan removals, and warnings. |

Prune deletes the ledger row first, then removes artifact directories best
effort. Unsafe artifact paths and remove failures are reported as warnings after
the ledger row is deleted.

### `cr data purge`

```text
cr data purge --yes [--json]
cr data purge --dry-run [--json]
```

Purges the whole local data root. `--yes` is required unless `--dry-run` is set.
Purge does not open the ledger database, so it can remove a corrupt local data
root. `--json` emits the data root, dry-run status, and removed status.

Flags:

| Flag | Semantics |
|------|-----------|
| `--yes` | Confirm permanent deletion. Required unless `--dry-run` is set. |
| `--dry-run` | Report the data root without deleting. |
| `--json` | Emit JSON. |

## Operational Semantics

### Local State

`cr` keeps non-secret config in the OS config directory for service
`codereview`. Durable review data lives under the OS data directory for
the `cr` binary and includes:

- `ledger.db`: runs, sessions, findings, planned actions, and named sessions;
- `runs/...`: per-run artifacts such as `diff.patch`, `findings.json`,
  `rollup.md`, diff slices, and agent JSONL logs;
- `locks/...`: live-review advisory locks.

The HTTP cache is under the OS cache directory for the `cr` binary.

Releases before this state-layout alignment wrote data/cache below a nested
`codereview` child inside the `cr` root. Commands that write review state
migrate those legacy entries into the `cr` root without changing config or
credential refs.

### Posting, Markers, And Idempotency

Live posting records hidden `codereview` markers on comments/reviews created by
the posting identity. Markers let later invocations classify the PR as complete,
partial, stale-base, or fresh for the current head/base context. The outbox
reconciles markers only from the posting identity, so unrelated comments from
other users do not satisfy `cr`'s idempotency checks.

Planned actions can include inline comments, file-level or rollup comments,
review submission, thread summary replies, and thread resolution. Thread
summary content uses separate summary markers. Model-generated marker-looking
text is escaped before posting.

### Session Reuse

`--session <name>` stores and reuses the provider session for live orchestrator
turns. A named session is scoped by name, profile, provider, adapter, model, and
host. Profile/provider/adapter/model mismatches are errors. Host mismatches warn
and continue. If the active adapter does not support resume, `cr` starts fresh
and records the new provider session when available. `claude_cli` supports
resume by launching a new Claude Code background job with the stored provider
session.

### Retention

`cr review` applies retention before fetching the PR or allocating the next run
when `data.retention.enforcement` is `at_write`. `data.retention.max_age_days`
controls live-run retention and defaults to 90 days. A value of `0` keeps live
runs forever. Dry-run runs remain throwaway data and use a fixed 7-day
review-time retention cap.

Set `data.retention.enforcement: manual_only` to disable review-time pruning for
both live and dry-run invocations. The explicit `cr data prune --dry-run`
command remains the safe preview path for manual maintenance, and no-selector
`cr data prune` uses its built-in live 90-day and dry-run 7-day windows
independent from profile config.

## Development

```bash
make tidy
make lint
make test
make build
make snapshot
make check
```

The main binary lives in `cmd/cr`. Command wiring is in `internal/cmd`, provider
boundaries are under `internal/gitprovider`, and review orchestration is split
between `internal/pipeline`, `internal/reviewrun`, `internal/reviewplan`, and
`internal/outbox`.

See [docs/development.md](docs/development.md) for local development notes.
