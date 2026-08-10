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
- reuse a fixed PR-scoped reviewer cohort and its LLM sessions by default;
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

`cr` stores secrets in explicit credential stores through the shared
`cli-common/credstore` library. A built-in `local-os` store is always available
for the current user.

| Platform | Built-in `local-os` backend |
|----------|----------------------------|
| macOS | Keychain |
| Windows | Credential Manager |
| Linux | Secret Service |

Configured stores can also target `1Password` desktop, service account, or
Connect backends, `pass`, encrypted file storage, or in-memory storage. Configure
additional stores from interactive `cr init` under **Configure secrets
storage**. The built-in `local-os` store is read-only configuration and cannot
be removed.

Interactive `cr init` treats credential stores, repository access, LLM runtimes,
reviewer entities, and review profiles as separate reusable building blocks.
Configure repository access to define how `cr` accesses Git repositories as the
current user; review profiles then select one configured repository-access entry
instead of editing Git credentials inline.

Interactive `cr init` discovers available secrets backends by default so it can
offer local 1Password account/vault choices and passive backend availability
checks. Use `cr init --secret-backend-discovery=safe` to skip active inventory
probes, or `--secret-backend-discovery=off` to skip backend discovery entirely.
The `CR_SECRET_BACKEND_DISCOVERY` environment variable accepts the same
`full`, `safe`, and `off` values when the flag is not supplied.

Secrets are never written to `config.yml`. Non-secret config lives in the
`codereview` config directory resolved by the operating system, and durable
review data lives under the OS data directory for the `cr` binary.

For repo review guidance, reviewer-facing dossier context, and dossier/workbench
retention conventions, see [docs/review-guidance.md](docs/review-guidance.md).
For first runs, reruns, fresh sessions, thread checkpoints, and interrupted-run
recovery, see [docs/review-lifecycle.md](docs/review-lifecycle.md).

## Authentication And Setup

`cr` supports GitHub (personal access token or GitHub App) and GitLab
(personal access token) authentication for Git-host operations. The credential
key for Git-host PATs is `git_token`.

Quick setup with adapter-managed LLM credentials:

```bash
cr init --non-interactive \
  --git-token-from-env GITHUB_TOKEN
```

Setup with Pi's local RPC runtime. Install Pi's coding agent and make sure the
`pi` binary is available on `PATH` before running `cr review`. New installs
should use the current npm package (`@earendil-works/pi-coding-agent`); existing
installs from the previous npm scope can also work when CR's compatibility
preflight confirms the reviewer controls it requires: RPC/system-prompt mode;
`--no-builtin-tools` with an exact `--tools` allowlist; explicit `--extension` loading while
`--no-extensions` disables discovery; and `--no-context-files`, `--no-approve`,
`--no-skills`, `--no-prompt-templates`, `--no-themes`, and `--no-session`.
CR preflights these capabilities before starting a Pi reviewer and returns an
incompatible-runtime error when any control is unavailable.

```bash
cr init --non-interactive \
  --llm-provider pi \
  --llm-auth subscription \
  --llm-adapter pi_rpc \
  --git-token-from-env GITHUB_TOKEN
```

Setup with Codex CLI subscription auth. Install Codex CLI, log in with your
subscription, and make sure the `codex` binary is available on `PATH` before
running `cr review`. This path is supported for review, but remains
best-effort/beta until Codex exposes an explicit all-tools-disabled flag.

```bash
cr init --non-interactive \
  --llm-provider openai \
  --llm-auth subscription \
  --llm-adapter codex_cli \
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

Repo-aware profile routing can select a profile from the PR repository when
`--profile` is omitted. Use this when org, personal, or repo-specific reviewer
credentials should differ without changing commands:

For scripted installs, use `cr init --non-interactive` for the core profile and
then layer narrow config mutations with the dedicated config commands. A typical
sequence looks like:

```bash
cr --profile work init --non-interactive \
  --git-host github.com \
  --git-auth-mode pat \
  --llm-provider anthropic \
  --llm-auth subscription \
  --llm-adapter claude_cli \
  --llm-reviewer-model-tier medium

cr --profile work config route set \
  --host github.com \
  --namespace open-cli-collective \
  --repo codereview-cli

cr --profile work config agent-source add ~/.config/codereview/agents
cr --profile work config llm models set medium claude-sonnet-4-6
cr config retention set --max-age-days 30 --enforcement manual_only
printf '%s' "$GITHUB_TOKEN" | cr set-credential \
  --store local-os \
  --name codereview/work \
  --key git_token \
  --stdin \
  --overwrite
```

Use `cr config route` for repository routing, `cr config agent-source` for
trusted source paths, `cr config llm models` for `llm.model_map`, and
`cr config retention` for durable run-data policy. Use interactive `cr init` to
configure additional credential stores. Use `--disable-reviewer` to remove
separate reviewer credentials, `--llm-reviewer-model-tier` or
`--clear-llm-reviewer-model-tier` for the durable reviewer baseline.

```yaml
repository_profiles:
  - profile: personal-reviewer-a
    match:
      host: github.com
      namespace: rianjs
      repos: [bar, baz]
  - profile: personal-reviewer-b
    match:
      host: github.com
      namespace: rianjs
      repos: [bar, baz]
  - profile: work
    match:
      host: github.com
      namespace: open-cli-collective
  - profile: personal
    match:
      host: github.com
      namespace: rianjs
profiles:
  personal: ...
  personal-reviewer-a: ...
  personal-reviewer-b: ...
  work: ...
```

Route matching is deterministic: `host + namespace + repo` routes beat
`host + namespace` routes, omitted `repos` means all repos in that namespace,
and no match fails with an actionable error asking for `--profile` or a route.
The same route may be listed for multiple profiles; if an omitted `--profile`
matches more than one profile at the winning specificity, `cr` fails fast and
lists the valid `--profile` choices instead of selecting a hidden default.
Host matching is case-insensitive after normalization, while namespace and repo
matching are case-sensitive after trimming whitespace. An explicit `--profile`
bypasses repository routing. Route targets still use the profile's configured
auth mode. Passing `--profile ""` is invalid.
For GitHub App reviewer entities, the review profile chooses whether `cr review`
discovers the installation from each PR owner/repo or uses one pinned
installation ID stored in profile config. Discovery is the normal choice for
profiles routed to multiple organizations or users; pinning is only appropriate
when every route for that profile uses the same installation.

Add or replace one credential later:

```bash
printf '%s' "$GITHUB_TOKEN" | cr set-credential \
  --store local-os \
  --name codereview/default \
  --key git_token \
  --stdin \
  --overwrite
```

Credential names use the visible `codereview/<name>` form. `cr` accepts secrets
by `--stdin` or `--from-env` during setup and credential writes; it does not
read runtime tokens directly from arbitrary environment variables. Reviewer
credentials use a separate credential name that also stores key `git_token`;
keep it distinct from the user Git credential in the same store.

### GitLab

GitLab merge requests (gitlab.com and self-managed instances) are reviewed
with the same commands as GitHub pull requests. A GitLab profile sets
`git.provider: gitlab` and authenticates with a personal access token holding
the `api` scope, stored under the same `git_token` credential key:

```yaml
profiles:
  gitlab-work:
    git:
      provider: gitlab
      host: gitlab.example.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/gitlab-work
```

```bash
printf '%s' "$GITLAB_TOKEN" | cr set-credential \
  --store local-os \
  --name codereview/gitlab-work \
  --key git_token \
  --stdin

cr review https://gitlab.example.com/group/subgroup/project/-/merge_requests/42
```

`git.provider` defaults to `github` when omitted, so existing configs are
unaffected. GitLab specifics worth knowing:

- Inline findings post as diff discussions and thread resolution uses the
  discussions API, so `resolve_threads` works as on GitHub.
- GitLab has no review-object equivalent of a GitHub review: an `approve`
  verdict calls the approvals API and the review summary posts as a
  merge-request note; `request_changes` revokes any active approval by the
  posting identity before posting the summary.
- `pat` is the only supported `auth_mode` for GitLab; there is no GitHub App
  equivalent.
- The `cr init` wizard does not offer GitLab yet — add the profile to
  `config.yml` directly (validated on load) and store the token with
  `cr set-credential`.
- Reading merge-request diffs requires GitLab 15.7 or newer for the raw diff
  endpoint; older instances fall back to per-file diff reconstruction.

### Org Deployment / MDM

For managed rollouts, ship non-secret config and pre-stage secrets in the
target user's credential store. The staging commands must run as the target OS
user who will run `cr`, not as root, SYSTEM, or an administrator account whose
keyring is different from the target user's keyring.

`reviewer_credentials` may use a PAT or GitHub App auth. A shared reviewer PAT
is an access secret even when distributed org-wide. A GitHub App private key is
also a deployment secret; `cr` signs short-lived JWTs locally, mints
installation tokens as needed, and never stores those minted tokens in
`config.yml`. Pre-stage reviewer credentials into each target user's credential
store, rotate or revoke them with the same `set-credential --overwrite` flow,
and do not store them in config files, agent sources, installers, logs, or
shell profiles.

For GitHub App reviewer credentials, grant the app only the repository
permissions needed by the enabled review workflow:

| GitHub App permission | Access | Used for |
|-----------------------|--------|----------|
| Metadata | Read | Required by GitHub for repository access and installation lookup. |
| Contents | Read | Reading diffs, files, and tree entries during review. |
| Pull requests | Read and write | Reading PR metadata/reviews/threads and posting PR reviews or inline comments. |
| Issues | Read and write | Reading and posting issue comments on PR conversations. |

Thread resolution uses the pull request review-thread GraphQL surface, so keep
Pull requests write access when `review_policy.resolve_threads` is enabled.
GitHub Apps can still hit a platform limitation here: they can post reviews,
inline replies, and summary comments, but GitHub may refuse thread resolution
with `Resource not accessible by integration`. In that case `cr` keeps the
posted review artifacts, warns, and relies on the summary comment metadata as
the durable cached-review artifact.

Example non-secret config template:

```yaml
secrets:
  stores:
    work-1password:
      display_name: 1Password Work
      backend:
        kind: op-desktop
        onepassword:
          account_url: signalft.1password.com
          vault_name: Employee
profiles:
  work:
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/work
    reviewer_credentials:
      auth_mode: github_app
      credential:
        store: work-1password
        name: codereview/work-reviewer-app
    llm:
      provider: anthropic
      auth: api_key
      adapter: anthropic_api
      credential:
        store: work-1password
        name: codereview/work-llm
      model_map:
        medium: claude-sonnet-4-6
    agent_sources:
      - ~/.config/codereview/agents
    review_policy:
      major_event: request_changes
      resolve_threads: auto
```

For adapter-managed LLM credentials, use `auth: subscription` and omit
`llm.credential`. Codex-backed profiles must set `provider: openai`,
`auth: subscription`, and `adapter: codex_cli` together. Pi-backed profiles
must set `provider: pi`, `auth: subscription`, and `adapter: pi_rpc` together:

```yaml
llm:
  provider: openai
  auth: subscription
  adapter: codex_cli
```

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

Agent `index.yaml` files must declare exactly one model selector and an
explicit effort:

```yaml
name: secrets
description: Reviews credential and secret-handling changes.
model_tier: medium
effort: medium
file_globs:
  - "**/*.go"
applies_when:
  - Go files changed
required_on_match: true
needs_full_file_content: false
```

Use `model_tier: small|medium|large` for portable shared catalogs. It means the
minimum acceptable reviewer tier for that agent, not a direct model pick. Use
`model_id: <provider-model-id>` only when an agent intentionally requires one
provider-specific model. `effort` is independent and must be one of
`low`, `medium`, or `high`.

`applies_when` is the selector's routing contract. Keep it focused on when the
agent should be chosen for a change. Reviewer execution instructions belong in
`prompt.md` and are not sent to the selector.

Set `required_on_match: true` when an agent must run whenever `file_globs`
matches a changed file. This applies to profile, repo, and `--agents-dir`
sources; invalid globs fail catalog loading, and an explicit `--max-agents`
smaller than the combined set of applicable repo-local and matching
`required_on_match` reviewers fails rather than dropping reviewers.

Legacy agent files that use `model: sonnet` or another provider-specific
`model` value must be updated. Replace portable intent with `model_tier`
and move provider-specific defaults into profile `llm.model_map`; use
`model_id` only for intentionally non-portable agents.

`cr config show --json` reports each configured source by path, presence,
status, canonical path, warnings, and SHA-256 fingerprint prefix without
inlining `index.yaml` or `prompt.md`. Missing or unreadable sources are shown as
deployment status in `config show`; commands that actually load agents, such as
`cr agents` and `cr review`, fail fast until the source is fixed.

Avoid profile sources under temp directories or relative paths. Git-backed
catalogs are allowed, but PR-shaped commands reject profile sources whose
canonical path resolves inside the current invocation worktree for the review
command because that checkout may be author-controlled. Other Git worktrees stay
allowed with provenance warnings. Symlinked sources are accepted, and `cr`
records the canonical path plus fingerprints of the resolved files.

When deploying a profile without the interactive wizard, run
`cr init --non-interactive` first to write the core non-secret profile shape,
then use `set-credential` only for secrets that you are intentionally staging
outside init. Example:

GitHub App reviewer credentials store the App ID and private key only. The
review profile stores installation routing: `discover_from_repository` for PR
context lookup, or `pinned` with one numeric installation ID when every route for
that profile shares the same installation.

```bash
cr --profile work init --non-interactive \
  --git-host github.com \
  --git-credential-ref codereview/work \
  --reviewer-auth-mode github_app \
  --reviewer-github-app-id "$GITHUB_APP_ID" \
  --reviewer-credential-ref codereview/work-reviewer-app \
  --llm-provider anthropic \
  --llm-auth api_key \
  --llm-adapter anthropic_api \
  --llm-credential-ref codereview/work-llm

printf '%s' "$USER_GITHUB_TOKEN" | cr set-credential \
  --store local-os \
  --name codereview/work \
  --key git_token \
  --stdin \
  --overwrite

printf '%s' "$GITHUB_APP_PRIVATE_KEY" | cr set-credential \
  --store local-os \
  --name codereview/work-reviewer-app \
  --key github_app_private_key \
  --stdin \
  --overwrite

printf '%s' "$ANTHROPIC_API_KEY" | cr set-credential \
  --store local-os \
  --name codereview/work-llm \
  --key anthropic_api_key \
  --stdin \
  --overwrite
```

Then apply any config that intentionally lives outside init flags:

```bash
cr --profile work config route set \
  --host github.com \
  --namespace open-cli-collective \
  --repo codereview-cli

cr --profile work config agent-source add ~/.config/codereview/agents
cr --profile work config llm models set medium claude-sonnet-4-6
cr config retention set --max-age-days 30 --enforcement manual_only
```

Then verify the deployed profile:

```bash
cr config show --json
cr me --all --json
```

Use `set-credential` only for deferred or multi-key credential bundles that are
clearer to manage outside init.

Environment variables in the examples above are setup ingress only. Runtime
commands resolve service credentials from `config.yml` and the configured
credential backend.

## Configuration

Run `cr config show` to inspect the active profile and credential status.

```yaml
profiles:
  default:
    fast: false
    git:
      host: github.com
      auth_mode: pat
      credential:
        store: local-os
        name: codereview/default
    llm:
      provider: anthropic
      auth: subscription
      adapter: claude_cli
      reviewer_model_tier: small
    agent_sources:
      - ~/.config/codereview/agents
    review_policy:
      major_event: request_changes
      allow_self_approve: false
      resolve_threads: auto
data:
  retention:
    max_age_days: 90
    enforcement: at_write
```

### Lifecycle hooks

Profiles may run observe-only lifecycle commands. Commands receive one JSON
payload on stdin, execute directly from `argv` without a shell, and never alter
the run outcome. They start asynchronously; `cr` drains commands still running
at exit, with each invocation bounded by its timeout.

```yaml
profiles:
  default:
    hooks:
      - event: reviewer.completed
        argv: ["/usr/local/bin/review-notify", "--reviewer-done"]
        timeout: 10s
        on_dry_run: true
      - event: run.completed
        argv: ["/usr/local/bin/review-notify", "--finished"]
        # timeout defaults to 30s; on_dry_run defaults to false
```

Review events are `run.started`, `workspace.prepared`, `dossier.ready`,
`selection.completed`, `reviewer.completed`, `plan.ready`, `posting.action`,
`run.completed`, and `run.failed`. Respond exposes only matching lifecycle
events: `respond.run.started`, `respond.plan.ready`,
`respond.posting.action`, `respond.run.completed`, and `respond.run.failed`.
The `benchmark.*` namespace is reserved and benchmark commands do not dispatch
hooks.

Every payload contains `event`, `pr_url`, `run_id`, `profile`, `pass_number`
(the ledger attempt), `artifact_dir`, and `dry_run`; terminal events also contain
`outcome`. `author` carries the pull request author's git-host login (GitHub
login, GitLab username) from the moment the run reads the pull request, which is
every event after `run.started`, and is omitted when a run fails before that
read. It is observed from the snapshot the run already fetches, so a hook that
addresses the author costs no extra host call. `reviewer.completed` adds
`reviewer_id` and `reviewer_status`,
`posting.action` adds `action_kind` and the canonical `action_marker` when the
provider call carries one, and `selection.completed` adds sorted `agents` plus
an agent-to-model `models` object. Values not yet allocated at an early event,
such as the run ID at `run.started`, are empty. The common fields are also
available as `CR_EVENT`, `CR_PR_URL`, `CR_RUN_ID`, `CR_AUTHOR`, `CR_OUTCOME`,
`CR_PROFILE`, `CR_PASS_NUMBER`, `CR_ARTIFACT_DIR`, and `CR_DRY_RUN`.

Missing commands, non-zero exits, and timeouts write warnings to stderr with
combined command output capped at 8 KiB. They never change the pipeline result
or exit code. During dry-run, only entries with `on_dry_run: true` run, and only
for lifecycle moments the dry-run actually reaches.

Supported values:

| Field | Values |
|-------|--------|
| `git.auth_mode` | `pat` and `github_app` are implemented for GitHub. `oauth_device` is reserved; config recognizes the mode but validation rejects it in v1. |
| `llm.provider` | `anthropic`, `openai`, `pi` |
| `llm.auth` | `subscription`, `api_key` |
| `llm.adapter` | `claude_cli`, `anthropic_api`, `openai_api`, `pi_rpc`, and `codex_cli` are usable for review. `codex_cli` requires `provider: openai` and `auth: subscription`, and is currently best-effort/beta because Codex does not yet expose an explicit all-tools-disabled flag. |
| `llm.model_map` keys | `small`, `medium`, `large` |
| `llm.max_effort` keys | `small`, `medium`, `large`; values `low`, `medium`, `high` |
| `llm.reviewer_model_tier` | `small`, `medium`, `large` |
| `review_policy.major_event` | `comment`, `request_changes` |
| `review_policy.resolve_threads` | `auto`, `never` |
| `data.retention.enforcement` | `at_write` applies review-time pruning before each `cr review`; `manual_only` disables review-time pruning and leaves `cr data prune` as the explicit maintenance path. Review-time pruning runs only when no other cr instance is mid-run (it requires the data-root active-runs lock exclusively) and is skipped, not queued, on contention. |

`subscription` LLM auth means the adapter owns its own credentials, such as a
logged-in CLI or local runtime. `api_key` LLM auth requires `llm.credential`
and stores a provider-specific API key in the selected credential store.

Reviewer agents use provider-neutral `model_tier` values as minimum floors. At
runtime, `cr` combines the profile's `llm.reviewer_model_tier` baseline with
the agent floor and resolves the higher tier through the active profile's
`llm.model_map` plus provider+adapter built-ins. User `llm.model_map` entries
override built-ins, and model IDs are opaque strings so future provider models
can be configured without a CLI update.

Effective reviewer tier is:

```text
max(llm.reviewer_model_tier, agent.model_tier)
```

When `llm.reviewer_model_tier` is omitted, `cr` defaults it to `small`.
`model_id` stays an exact provider-specific selection and does not participate
in the tier-floor calculation.

Migration note: older releases treated reviewer `model_tier` as a direct map
lookup. Current releases treat it as a minimum acceptable tier, so profiles can
raise the reviewer baseline without editing shared agent catalogs.

### Model-Tier Floors and Effort Ceilings

Agent catalogs declare an absolute `effort` (`low`, `medium`, `high`) that
becomes the provider's reasoning-effort setting. Model tiers are different:
`agent.model_tier` and `llm.reviewer_model_tier` are minimum floors for model
selection, while `llm.max_effort` is a ceiling for the default effort at the
resolved tier. They do not raise an agent's effort or select a model by
themselves.

For a tier-based stage, `cr` applies this order:

1. Resolve the effective tier as the higher of the agent tier and the profile
   reviewer-tier floor.
2. Resolve `model_map[effective tier]`, including provider built-ins.
3. Cap the agent or stage default effort with `max_effort[effective tier]`.
4. Apply an explicit effort override, which wins over the ceiling.

Configure ceilings manually in `config.yml`:

```yaml
llm:
  model_map:
    large: openai-codex/gpt-5.6-sol
    medium: openai-codex/gpt-5.6-terra
  max_effort:
    large: medium
```

A tier absent from `max_effort` is uncapped. The cap is a ceiling only: an agent
declaring `low` under a `medium` ceiling still runs at `low`. Caps use the tier
after floor resolution and apply to internal stages (selection, synthesis, and
thread analysis) as well as reviewers.

The complete precedence and bypass table is:

| Input or path | Model selection | Default effort | `llm.max_effort` |
|---------------|-----------------|----------------|-----------------|
| Agent/stage tier with no explicit override | Effective tier after floors, then `model_map` | Agent/stage effort | Caps the default at the effective tier |
| `--reviewer-model-tier` | Raises the reviewer baseline before the agent floor is applied | Agent effort | Caps at the final resolved tier |
| `--selection-effort` or `--reviewer-effort` | Normal tier or exact-model selection | Requested effort | Explicit effort wins after the ceiling |
| `--selection-model` or `--reviewer-model` | Exact requested model ID | Stage/agent effort or explicit effort | Bypassed; no tier is available |
| Agent `model_id` | Exact agent model ID | Agent effort | Bypassed; no tier is available |
| `cr benchmark run` stage model/effort overrides | Exact benchmark model when supplied; otherwise normal tier selection | Explicit benchmark effort when supplied | Explicit benchmark overrides bypass the profile ceiling |

For example, with `agent.model_tier: small`, `effort: high`,
`llm.reviewer_model_tier: large`, and `max_effort.large: medium`, the reviewer
runs with the large model at medium effort. Adding `--reviewer-effort high`
runs that same large model at high effort. Adding
`--reviewer-model my-provider/model` selects that exact model and keeps high
effort without applying the tier ceiling. `--selection-effort high` follows
the same post-ceiling override rule for selection.

`cr init` preserves `max_effort` but cannot yet edit it; set it by hand in
`config.yml`.

Dry-run and no-post runs also record selected reviewer runtime resolution in
`agent-sources.json` for auditability. Each selected agent may include
`reviewer_runtime` with:

| Field | Meaning |
|-------|---------|
| `mode` | `tier_floor` for portable tier resolution, `exact_model` for agent `model_id` passthrough, or `override` for `--reviewer-model` |
| `floor_tier` | Declared agent `model_tier` floor when `mode=tier_floor` |
| `baseline_tier` | Effective operator baseline tier used for this run |
| `effective_tier` | Higher of baseline and agent floor |
| `resolved_model` | Actual provider model, from the active model map for `tier_floor`, agent `model_id` for `exact_model`, or `--reviewer-model` for `override` |
| `resolved_effort` | Actual reviewer effort after the tier ceiling and any explicit reviewer-effort override |
| `model_map_source` | `built_in` or `config` for the resolved tier mapping |

Built-in model maps:

| Provider | Adapter | small | medium | large |
|----------|---------|-------|--------|-------|
| `openai` | `codex_cli` | `gpt-5.4-mini` | `gpt-5.4` | `gpt-5.5` |
| `openai` | `openai_api` | `gpt-5.4-mini` | `gpt-5.4` | `gpt-5.5` |
| `anthropic` | `claude_cli` | `claude-haiku-4-5` | `claude-sonnet-5` | `claude-opus-5` |
| `anthropic` | `anthropic_api` | unset | unset | unset |
| `pi` | `pi_rpc` | unset | unset | unset |

`anthropic_api` and `pi_rpc` require explicit `llm.model_map` entries for every
tier an agent asks for; an unmapped tier fails the run and names the entry to
add. A built-in mapping is a floor, not a recommendation: override the tier in
`llm.model_map` when a stage deserves a stronger model than its tier implies.

For Anthropic subscription profiles, `adapter: claude_cli` runs Claude Code
background jobs, writes the full review task to `cr-prompt.txt` in an
adapter-owned scratch directory, and starts `claude --bg` with a compact
positional prompt that allows only `Read,Write`. The adapter reads the review
result from `cr-result.json` in that scratch directory and records the Claude
provider session returned by the job state. Background jobs run from a stable
cache workdir so Claude Code can resolve resumed sessions consistently; set
`CR_CLAUDE_BG_WORK_DIR` to override that workdir. For checkout-native reviewer
execution, Claude CLI receives a disposable reviewer workspace, run-owned
scratch/temp/cache directories, and `Read,Write,Bash` tools through Claude Code
directory permissions.

For OpenAI subscription profiles, `adapter: codex_cli` runs `codex exec` in an
adapter-owned scratch directory with `--json`, `--ephemeral`,
`--skip-git-repo-check`, `--ignore-user-config`, `--ignore-rules`, a read-only
sandbox, and a scratch `--cd`. For checkout-native reviewer execution, Codex CLI
uses `workspace-write` with the disposable reviewer workspace as `--cd`, plus
run-owned scratch/temp/cache environment variables for verification commands.
The adapter rejects unsafe flags. Non-reviewer Codex requests remain
best-effort/beta because Codex CLI does not yet expose a first-class
all-tools-disabled flag.

Credential key matrix:

| Profile field | Purpose | Auth/provider | Required keys | Optional keys | v1 behavior |
|---------------|---------|---------------|---------------|---------------|-------------|
| `git.credential` | User Git host auth | `pat` | `git_token` | None | Supported |
| `reviewer_credentials.credential` | Reviewer Git host auth | `pat` | `git_token` | None | Supported; must use a distinct credential location from `git.credential` in the same profile |
| `git.github_app.app_id` / reviewer entity `github_app.app_id` | Git host auth | `github_app` | Config field, not a credential key | None | Non-secret GitHub App ID stored in config. Scripted init uses `--git-github-app-id` or `--reviewer-github-app-id` |
| `git.credential` / reviewer entity credential | Git host auth | `github_app` | `github_app_private_key` | None | Supported for GitHub. Reviewer GitHub App installation routing lives on `profiles.<name>.reviewer.github_app_installation`, not in the credential bundle |
| `git.credential` / `reviewer_credentials.credential` | Git host auth | `oauth_device` | None | None | Reserved; config recognizes the mode but v1 rejects it and does not accept future keys such as `git_oauth_access_token` or `git_oauth_refresh_token` |
| `llm.credential` | Anthropic direct API auth | `api_key` + `anthropic` | `anthropic_api_key` | None | Supported |
| `llm.credential` | OpenAI direct API auth | `api_key` + `openai` | `openai_api_key` | None | Supported |
| Omitted `llm.credential` | Adapter-managed LLM auth | `subscription` + `anthropic`/`openai`/`pi` | None | None | Supported; credentials are owned by the selected adapter. `openai + codex_cli` is best-effort/beta until Codex exposes an explicit all-tools-disabled flag |
| `llm.credential` | Pi direct API auth | `api_key` + `pi` | None | None | Unsupported; use adapter-managed `subscription` auth with `pi_rpc` |

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

The PR URL and selected profile are the repository inputs. `cr review` may be
run outside a Git checkout or from an unrelated repository; it fetches the
PR's exact base and head commits into the run-owned workbench using the
profile's repository credential.

Run a live review and fail the command when a major or blocking finding exists:

```bash
cr review --fail-on major https://github.com/OWNER/REPO/pull/123
```

Force a fresh local live review instead of using existing approval, override,
resume, or marker gates while continuing the PR's orchestrator and reviewer
sessions:

```bash
cr review --rerun https://github.com/OWNER/REPO/pull/123
```

Start both a fresh local review and fresh orchestrator and reviewer
conversations, including reviewer reselection:

```bash
cr review --rerun --fresh-session https://github.com/OWNER/REPO/pull/123
```

Retry missing or failed required posts without rerunning the LLM review:

```bash
cr review --retry-posts https://github.com/OWNER/REPO/pull/123
```

Approve after a prior agentic pass when the PR author explicitly asks to skip a
low-value rerun:

```bash
# Comment on the PR after the latest codereview marker, for example:
# "These findings are low-value; please approve the pull request."
cr review https://github.com/OWNER/REPO/pull/123
```

By default, dry-run and live reviews of the same PR, profile, and posting
identity reuse one orchestrator session and one PR-scoped reviewer cohort
across pushes. Override only the orchestrator scope with a named session for a
series of related live reviews:

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
cr benchmark select .codereview/benchmarks/reviewer.yml \
  --candidate claude-sonnet-medium \
  --case merged-security-pr
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
| `--profile <name>` | Select a configured profile and bypass repo-aware routing. When omitted, PR-aware commands use `repository_profiles`; commands that cannot resolve a route require `--profile`. During `init`, an omitted profile starts with the suggested name `default`. |
| `--backend <name>` | Compatibility/runtime backend selector. It cannot override an explicit credential store destination. |
| `--quiet` | Suppress structured progress logs on stderr. Command stdout and stderr error reporting stay unchanged. |

Progress-bearing commands emit one-line structured events on stderr in the
form `cr progress event=<start|finish|error> command="..." op="..." target="..." ...`.
This output is intended for human progress visibility and simple substring
parsers. JSON and text results continue to write to stdout only. In this slice,
structured progress is enabled for `benchmark run`, `benchmark select`,
`benchmark compare`, `data prune`, `data purge`, `config clear`, and
`sessions delete`.

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

Creates or updates non-secret config. If the selected profile already exists,
pass `--replace-profile` to replace the profile config.

Without `--non-interactive`, `cr init` opens an interactive workspace builder.
The main menu stages changes in reusable areas before a final commit:

1. Configure secrets storage
2. Configure repository access
3. Configure LLM runtimes
4. Configure reviewer entities
5. Configure review profiles
6. Configure global settings

Review profiles compose repository access, reviewer entity, and LLM runtime
selections. Repository access must be configured before creating a review
profile.

Flags:

| Flag | Semantics |
|------|-----------|
| `--non-interactive` | Run without prompts. |
| `--git-host <host>` | Git host, default `github.com`. The PR host must match this value. |
| `--git-credential-ref <name>` | Credential name for Git auth. Defaults to `codereview/<profile>`. |
| `--git-token-stdin` | Read the Git token from stdin and write key `git_token`. |
| `--git-token-from-env <env>` | Read the Git token from an environment variable and write key `git_token`. |
| `--reviewer-credential-ref <name>` | Credential name for reviewer Git auth. Defaults to `codereview/<profile>-reviewer` when reviewer credentials are requested. |
| `--reviewer-auth-mode <mode>` | Reviewer Git auth mode, default `pat`. `github_app` writes config and prints follow-up credential commands. |
| `--reviewer-token-stdin` | Read the reviewer Git token from stdin and write key `git_token`; PAT reviewer auth only. |
| `--reviewer-token-from-env <env>` | Read the reviewer Git token from an environment variable and write key `git_token`; PAT reviewer auth only. |
| `--llm-provider <provider>` | LLM provider, default `anthropic`; also selects whether API-key ingress writes `anthropic_api_key` or `openai_api_key`. `pi` is adapter-managed and does not accept API-key ingress. |
| `--llm-auth <mode>` | LLM auth mode, default `subscription`. Use `api_key` for credential-store-managed direct API keys. |
| `--llm-adapter <adapter>` | LLM adapter, default `claude_cli`. |
| `--llm-credential-ref <name>` | Credential name for LLM API-key auth. Defaults to `codereview/<profile>-llm` when `--llm-auth api_key`. |
| `--llm-api-key-stdin` | Read the LLM API key from stdin and write `anthropic_api_key` or `openai_api_key` according to `--llm-provider`. |
| `--llm-api-key-from-env <env>` | Read the LLM API key from an environment variable and write `anthropic_api_key` or `openai_api_key` according to `--llm-provider`. |
| `--secret-backend-discovery <mode>` | Interactive secrets-backend discovery mode: `full` runs active inventory probes such as 1Password account/vault lookup, `safe` uses only passive availability checks, and `off` skips backend discovery. Defaults to `full`; `CR_SECRET_BACKEND_DISCOVERY` can set the same value when the flag is omitted. |
| `--agent-source <path>` | Add a trusted agent source directory. Repeatable. |
| `--major-event <policy>` | `comment` or `request_changes`. Controls review event for major findings. Defaults to `request_changes`. |
| `--allow-self-approve` | Store profile policy allowing self approval. Live review can still require `--allow-self-approve` depending on invocation. |
| `--resolve-threads <policy>` | `auto` or `never`. Defaults to `auto`. |
| `--overwrite` | Replace existing keyring entries written by this command. |
| `--replace-profile` | Replace an existing profile config. |

Only one stdin secret ingress flag may be used at a time. PAT reviewer
credentials use key `git_token` under their own credential name, so
`--reviewer-credential-ref` must differ from `--git-credential-ref`. GitHub App
reviewers store their non-secret App ID in config via `--reviewer-github-app-id`
and use `github_app_private_key` as the only credential-store key; installation
routing is profile config, not a credential key. `init` does not accept
reviewer token ingress for `--reviewer-auth-mode github_app`. LLM API-key ingress requires
`--llm-auth api_key`. `--overwrite` with API-key auth requires an LLM key
ingress flag. `--allow-self-review` is intentionally runtime-only on
`cr review`; `init` only stores the profile-level self-approval policy.

### `cr set-credential`

```text
cr set-credential --store <id> --name <credential-name> --key <key> (--stdin | --from-env <env>) [flags]
```

Writes one secret value to the selected credential store. Globally allowed keys are
`git_token`, `github_app_private_key`, `anthropic_api_key`, and
`openai_api_key`. When
`config.yml` declares the target credential location, `set-credential` narrows
that global allowlist to the exact key set expected for that credential. PAT
user Git credentials and PAT reviewer credentials use `git_token`; GitHub App
credentials use `github_app_private_key`; Anthropic
LLM API-key credentials use `anthropic_api_key`; OpenAI LLM API-key credentials use
`openai_api_key`.

Flags:

| Flag | Semantics |
|------|-----------|
| `--store <id>` | Required credential store id, such as `local-os` or a configured store. |
| `--name <credential-name>` | Required credential name, such as `codereview/default`. |
| `--key <key>` | Required key name. Git credentials accept the keys described in the credential key matrix; LLM API-key credentials accept `anthropic_api_key` or `openai_api_key`. |
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
credential locations, non-secret profile config, review policy, and data
retention. For each declared credential location, it reports whether expected
keys are present and labels optional keys as optional.
For each configured agent source, it reports deployment-material status,
canonical path, warnings, and SHA-256 fingerprint prefix without inlining agent
definition contents.

`--json` emits the same information as structured JSON.

### `cr config route`

```text
cr config route list [--json]
cr --profile <profile> config route set --host <host> --namespace <namespace> [--repo <repo> ...]
cr config route unset --host <host> --namespace <namespace> [--repo <repo> ...]
cr --profile <profile> config route unset --host <host> --namespace <namespace> [--repo <repo> ...]
```

Inspects and updates `repository_profiles` routing rules. `set` applies to the
active profile and preserves matching routes for other profiles, allowing shared
routes. `unset` removes matching routes for all profiles unless `--profile` is
supplied, in which case it removes only that profile's matching route. Omit
`--repo` to make the route namespace-wide, or repeat `--repo` for repo-specific
routes.

Use this command for scripted route parity instead of trying to encode routing
inside `cr init --non-interactive`.

### `cr config agent-source`

```text
cr config agent-source list [--json]
cr config agent-source add <path>
cr config agent-source remove <path>
```

Inspects and updates the active profile's `agent_sources`. `add` normalizes the
path, skips duplicates, and preserves unrelated sources. `remove` removes only
matching normalized paths from the active profile.

### `cr config retention`

```text
cr config retention get [--json]
cr config retention set [--max-age-days <days>] [--enforcement <policy>]
cr config retention reset
```

Inspects and updates global `data.retention`. `set` accepts a non-negative
`--max-age-days` value where `0` means keep forever, and an `--enforcement`
value of `at_write` or `manual_only`. `reset` restores the default `90` day
`at_write` policy.

### `cr config llm models`

```text
cr config llm models list [--json]
cr config llm models resolve <tier> [--json]
cr config llm models set <tier> <model>
cr config llm models unset <tier>
cr config llm models reset [--provider <provider>]
```

Inspects and updates the active profile's `llm.model_map`. `list` shows the
effective `small`/`medium`/`large` map, including whether each value came from
profile config, built-in defaults, or is unset. `resolve` prints the concrete
provider model ID for one tier under the active profile.

`set`, `unset`, and `reset` mutate only the active profile. `reset` clears the
configured map so built-ins apply again; `--provider` is a safety guard and
must match the active profile provider.

### `cr config clear`

```text
cr config clear [--all] [--dry-run] [--json]
```

Deletes stored credentials declared by the active profile.

Flags:

| Flag | Semantics |
|------|-----------|
| `--all` | Also remove the active profile from `config.yml` and clear the disposable cache root. Inactive profiles and durable data are not touched. |
| `--dry-run` | Report the credential locations, config profile reset, and cache cleanup that would happen without deleting credentials, config, cache, or data. |
| `--json` | Emit a JSON result. |

Without `--all`, this removes secret keyring entries only and leaves
`config.yml` intact. With `--all`, `--profile <name>` selects the profile to
reset; without a selected profile, the command fails. If the last profile is
reset, `config.yml` is removed.

`config clear` never touches durable review data. Use `cr data purge` for the
data pillar.

When progress logging is enabled, `config clear` brackets config load, profile
resolution, credential-store open, credential deletion, optional profile-config
removal, and optional cache cleanup on stderr. Partial-success failures still
render their JSON or text result on stdout before returning the final error.

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

Shows one agent, including category metadata, `model_tier` or `model_id`,
effort, file globs, `applies_when`, `required_on_match`,
`needs_full_file_content`, prompt,
provenance, and trust note when repo-local agents are considered.

### `cr review`

```text
cr review <PR> [flags]
```

Runs the automated review pipeline for a GitHub pull request URL or a GitLab
merge request URL. The PR host must match the active profile's `git.host`, and
the URL's provider family must match the profile's `git.provider`.

Modes:

| Flag | Semantics |
|------|-----------|
| `--dry-run` | Plan review actions, write local artifacts, and print the plan without posting. |
| `--no-post` | Alias for `--dry-run`. |
| `--rerun` | Bypass existing local approval, approval-override, resume, and marker gates and start a new live review while reusing the PR's original reviewer cohort plus reviewer and orchestrator sessions. Mutually exclusive with `--retry-posts`. |
| `--retry-posts` | Retry missing or failed required posts for an existing run without rerunning LLM planning or checking approval overrides. Mutually exclusive with `--rerun` and incompatible with `--session`. |
| `--fresh-session` | Reset the PR-scoped reviewer cohort, reselect reviewers, and start fresh reviewer and orchestrator conversations without changing local review gates. Incompatible with `--retry-posts`, which does not run LLM planning. |
| `--fast` | Enable fast execution for reviewer agents, overriding the profile default. Incompatible with `--retry-posts`. |
| `--no-fast` | Disable fast execution for reviewer agents, overriding the profile default. Mutually exclusive with `--fast`. |

Review selection and execution flags:

| Flag | Semantics |
|------|-----------|
| `--agents-dir <path>` | Additional trusted agents directory. Repeatable. |
| `--max-agents <n>` | Set a hard total reviewer limit. Omit the flag or pass `0` to run all applicable repo-local reviewers and all matching `required_on_match` reviewers, plus up to 5 optional shared reviewers. A positive value below that combined required set fails. Negative values are rejected. |
| `--max-concurrency <n>` | Limit concurrent reviewer agents. Omit the flag or pass `0` for the default limit of 5. Negative values are rejected. |
| `--selection-model <model>` | Exact provider model ID passthrough for the selection stage only. Bypasses the default medium-tier selection model resolution. Requires `--dry-run` or `--no-post`. |
| `--selection-effort <effort>` | Override selection-stage effort only with `low`, `medium`, or `high`. Requires `--dry-run` or `--no-post`. |
| `--selection-prompt <path>` | Load selection-stage instruction text from a file while preserving the structured JSON selection protocol. Requires `--dry-run` or `--no-post`. |
| `--reviewer-model <model>` | Exact provider model ID passthrough for reviewer stages only. Bypasses reviewer agent `model_tier`, `model_id`, and profile model-map resolution. Available for dry-run, no-post, and live reviews. |
| `--reviewer-model-tier <tier>` | Override the reviewer baseline tier only with `small`, `medium`, or `large`. This still respects higher agent `model_tier` floors. Requires `--dry-run` or `--no-post`. |
| `--reviewer-effort <effort>` | Override reviewer-stage effort only with `low`, `medium`, or `high`. Available for dry-run, no-post, and live reviews. |
| `--review-base-sha <sha>` | Review this base commit SHA instead of the PR's current base SHA. Requires `--review-head-sha` and `--dry-run` or `--no-post`. |
| `--review-head-sha <sha>` | Review this head commit SHA instead of the PR's current head SHA. Requires `--review-base-sha` and `--dry-run` or `--no-post`. |
| `--session <name>` | Override the PR's default orchestrator session with a named live-review session. Reviewer cohorts remain PR-scoped. Not allowed with `--dry-run`, `--no-post`, or `--retry-posts`. |

Review progress on stderr reports the merged reviewer catalog, final selected
IDs and reasoning, and each reviewer assignment with winning provenance and
file scope. `--quiet` suppresses these records.

Policy and output flags:

| Flag | Semantics |
|------|-----------|
| `--fail-on <severity>` | Exit 1 if any finding is at or above `blocking`, `major`, `minor`, or `nits`. |
| `--allow-self-review` | Allow reviewer credentials that resolve to the PR author. |
| `--allow-self-approve` | Allow approval when the posting identity is the PR author. |
| `--no-resolve-threads` | Do not plan thread-resolution actions. Also implied by profile `resolve_threads: never`. |
| `--json` | Emit JSON. |

Fast mode defaults off; set `fast: true` on a profile to enable it by default.
`--fast` and `--no-fast` override the profile. Unsupported runtime/model
combinations warn and continue at normal speed. Fast mode supports `claude_cli`
and `anthropic_api` with `claude-opus-5` or `claude-opus-4-8`, and `codex_cli`
with `gpt-5.4`, `gpt-5.5`, `gpt-5.6-sol`, `gpt-5.6-terra`, or `gpt-5.6-luna`.
Anthropic has removed Opus 4.7 fast mode. Claude CLI receives a per-session
`fastMode` setting, Anthropic API
requests use its fast-mode beta, and Codex CLI receives `service_tier="fast"`.
Fast mode has premium pricing and applies only to reviewer agents, not
selection, synthesis, approval-override classification, or `cr respond` thread
analysis.
Anthropic API fast mode is a research preview that requires account access;
requests without access fail as upstream errors during the run. Subscription
fast mode bills usage credits, requires owner enablement on Team and Enterprise,
and can silently degrade to standard speed when credits or fast capacity are
unavailable. Reviewer runtime artifacts therefore record the fast request,
whether it was ignored as unsupported, and the speed actually reported by the
provider, or `unknown` when unavailable.

Local run state and provider session state are independent. By default, each
PR/profile/posting-identity tuple gets one durable orchestrator session and one
ordered reviewer cohort whose members retain their own provider sessions. Plain
follow-up reviews and `--rerun` reuse that cohort and those exact sessions even
when the PR head changes. `--session` selects an explicit named orchestrator
session only; the reviewer cohort remains PR-scoped. `--fresh-session` clears
both scopes for the invocation, runs selection again, and persists the new
orchestrator session and reviewer cohort.

Live review uses a local gate before planning or posting. If the posting
identity has already approved the PR, `cr review` exits immediately in Go code
before any LLM classifier or reviewer loop runs; this is true even if newer
commits made the approval stale. Use `--rerun` to force a new local review
while still resuming the provider session, or combine it with
`--fresh-session` to make both layers fresh.

If there is no existing approval, `cr review` looks for an explicit PR-author
approval override request newer than the latest codereview marker from the
posting identity. This check only runs after at least one prior codereview marker
exists. Candidate comments are filtered in Go to PR-author issue comments,
review bodies, and inline-thread comments newer than that marker; a small-tier
LLM classifier only decides whether those comments ask for approval override. A
positive classification skips the reviewer loop and posts an approving review
through the normal ledger/outbox path. Only ordering relative to the latest
codereview marker is enforced; the tool intentionally does not require the
request to be newer than a later head update.

If matching complete markers already exist for the current
head/base/profile/posting identity, `cr review` exits early. If a prior
compatible partial run exists, it resumes or repairs posting from durable local
state. If the PR base moved, live review aborts with an upstream exit. `--rerun`
starts fresh and bypasses the approval/override fast paths. `--retry-posts`
replays required posts that are missing or failed without rerunning the LLM
review or checking approval overrides.

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
running reviews. For the current full-pipeline commands, this includes
stage-recipe validation and selection prompt file readiness checks.

### `cr benchmark doctor`

```text
cr benchmark doctor <suite.yml> [--candidate <id> ...] [--case <id> ...] [--results-dir <path>] [--cr-bin <path>] [--json]
```

Inspects benchmark readiness without running reviews. It reports the selected
candidates and cases, resolved results directory, selected `cr` binary, profile
availability, selection/reviewer/synthesis stage recipes, reviewer agent
directories, and warnings.

### `cr benchmark run`

```text
cr benchmark run <suite.yml> [--candidate <id> ...] [--case <id> ...] [--results-dir <path>] [--cr-bin <path>] [--json]
```

Runs the selected candidate x case matrix by invoking `cr review --dry-run
--json` for each run. It always uses dry-run review mode, captures stdout and
stderr separately, records each child exit code, and writes generated artifacts
under `.cr-bench/results/<suite-id>/<timestamp>/` unless `--results-dir` is set.

Use `--candidate` and `--case` for benchmark selection. Candidate YAML fields
such as `stages.selection`, `stages.reviewers`, `max_agents`, and
`max_concurrency` control review runtime overrides. `stages.selection.model`
and `stages.selection.effort` map to `--selection-model` and
`--selection-effort`; optional `stages.selection.prompt` maps to
`--selection-prompt`. `stages.reviewers.model` remains the exact-model bypass,
while `stages.reviewers.model_tier` maps to `--reviewer-model-tier`.
`stages.reviewers.effort` and `stages.reviewers.agent_dirs[]` map to
`--reviewer-effort` and repeated `--agents-dir` flags. Case YAML fields
`review_base_sha` and `review_head_sha` pin the exact base/head commit pair
reviewed by the dry-run child command. Optional `stages.synthesis` metadata is
reserved for future benchmark support and does not change `benchmark run`
execution yet.

`benchmark run` emits progress on stderr for suite load and selection, results
directory creation, each matrix run, child `cr review` subprocess execution,
suite artifact writes, and comparison artifact generation. Child review
subprocesses are invoked with `--quiet` so their internal progress does not
pollute captured benchmark stderr artifacts.

### `cr benchmark select`

```text
cr benchmark select <suite.yml> [--candidate <id> ...] [--case <id> ...] [--results-dir <path>] [--json]
```

Runs the selected candidate x case matrix through the extracted in-process
selection phase only. It reuses the same profile, provider, adapter, and agent
catalog loading semantics as `cr review`, but stops after selector execution.
It does not spawn a child `cr review`, does not run reviewer agents, and does
not run synthesis.

Candidate `stages.selection.model` and `stages.selection.effort` map to
selection overrides directly. Optional `stages.selection.prompt` is loaded from
disk into selection instructions, and optional
`stages.reviewers.agent_dirs[]` are passed into the selector catalog so
candidate reviewer packs still influence routing. Case YAML
`review_base_sha` and `review_head_sha` still pin the exact base/head commit
pair used to build the selector diff view.

Optional `stages.synthesis` metadata is preserved in summaries and selector
`recipe.json` artifacts, but selector-only benchmarking still stops after
selection. Dedicated synthesis benchmarking is future work.

Selector benchmarks write selector-specific per-run artifacts such as
`selection.json`, `recipe.json`, `metrics.json`, and selection log metadata.
When one selector run fails after partial execution, `benchmark select` records
that run as failed and continues the remaining matrix when it can still write
trustworthy suite artifacts.

`benchmark select` emits progress on stderr for suite load and selection,
results directory creation, per-profile runtime open, each selector run,
selection pipeline execution, and artifact generation.

### `cr benchmark compare`

```text
cr benchmark compare <results-dir> [--json]
```

Reads an existing benchmark results directory and writes deterministic
`comparison.json` and `comparison.md` artifacts. Comparison is local-only: it
does not invoke models, read live PR state, mutate Git provider state, or
require provider credentials. `cr benchmark run` and `cr benchmark select`
write the same comparison artifacts automatically.

`benchmark compare` emits progress on stderr while it loads the suite summary,
builds the comparison model, and writes JSON and Markdown artifacts.

### `cr sessions list`

```text
cr sessions list [--json]
```

Lists named orchestrator sessions in name order. Reviewer cohorts are automatic
PR-scoped state and are not listed here. Text output shows name, profile,
provider, adapter, model, host, and last-used time. JSON output includes the
provider session ID plus created and last-used timestamps.

### `cr sessions show`

```text
cr sessions show <name> [--json]
```

Shows one named orchestrator session. Missing sessions return an error. Text
output includes the provider session ID.

### `cr sessions delete`

```text
cr sessions delete <name> [--json]
```

Deletes one named orchestrator session row. It does not delete provider-side
session state or the PR-scoped reviewer cohort. Missing sessions return an
error.

`sessions delete` emits progress on stderr for layout resolution, legacy
migration, ledger open, and session deletion.

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
the ledger row is deleted. Orphan directories modified within the last 24 hours
are exempt from the sweep: an unreferenced directory can belong to a run that is
still being set up.

A live (non-dry-run) prune refuses while any review or respond run on the
machine holds the data-root active-runs lock, exiting with "another cr instance
appears to be running; wait for it to finish and retry". `--dry-run` never takes
the lock.

When progress logging is enabled, `data prune` emits stderr brackets for layout
resolution, optional legacy migration, ledger open, run selection, delete
actions, orphan discovery and removal, and the dry-run legacy check.

### `cr data purge`

```text
cr data purge --yes [--json]
cr data purge --dry-run [--json]
```

Purges the whole local data root. `--yes` is required unless `--dry-run` is set.
Purge does not open the ledger database, so it can remove a corrupt local data
root. `--json` emits the data root, dry-run status, and removed status. Like a
live prune, purge refuses while any review or respond run on the machine holds
the data-root active-runs lock.

`data purge` emits progress on stderr for layout resolution and the purge
action itself.

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
credential locations.

### Posting, Markers, And Idempotency

Live posting records hidden `codereview` markers on comments/reviews created by
the posting identity. Markers let later invocations classify the PR as complete,
partial, stale-base, or fresh for the current head/base context. The outbox
reconciles markers only from the posting identity, so unrelated comments from
other users do not satisfy `cr`'s idempotency checks.

Planned actions can include inline comments, file-level or rollup comments,
review submission, thread summary replies, and thread resolution. Thread
summary content uses separate summary markers. When GitHub App credentials
cannot resolve review threads, the summary comment still carries the metadata
used to observe and reuse cached review output. Model-generated marker-looking
text is escaped before posting.

### Session Reuse

Default and named session metadata lives in `ledger.db`'s existing
`named_sessions` table. The default key contains the PR identity, profile, and
posting identity, but not head or base SHAs. Both session kinds validate name,
profile, provider, adapter, model, and host before resume. A mismatch in the
derived default scope warns and starts fresh. An explicit named-session scope
mismatch aborts before invoking the LLM so the named context is not overwritten.
A host mismatch warns and continues. If the active adapter does not support
resume, `cr` starts fresh and records the new provider session when available.
`claude_cli` supports resume by launching a new Claude Code background job with
the stored provider session.

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
