# Init UX Contract

This document is the product-facing UX contract for the interactive `cr init`
experience. It complements [docs/init-config-surface.md](init-config-surface.md),
which owns durable config fields, mutation semantics, and non-interactive
surface rules.

Use this document to answer:

- what mental model `cr init` should teach
- what the interactive session should feel like
- which concepts are user-facing versus schema-facing
- how draft-only interactive state relates to the current `config.yml` schema

## Command Contract

- The public command remains `cr init`.
- The interactive experience may behave like a setup workspace, but it does not
  introduce a separate `setup` command.
- Interactive `init` may build up draft state across multiple screens and
  profiles before saving anything.

## Primary Terms

Interactive `init` should use these primary user-facing terms:

- **Git scope**: the Git host and main Git credentials that define where a
  review profile operates. Examples for v1 are `github.com` and GitHub
  Enterprise hosts such as `github.mycompany.com`.
- **Reviewer entity**: the actor that posts `COMMENT`, `APPROVE`, or
  `REQUEST_CHANGES` on pull requests.
- **LLM runtime**: the way reviewer agents run and authenticate, such as Claude
  CLI subscription auth, Codex CLI subscription auth, Pi local runtime, or a
  direct API-key-backed provider path.
- **Review profile**: one saved profile assembled from a Git scope, an LLM
  runtime, and a reviewer entity, plus its related review policy, routes, and
  optional advanced settings.

Supporting language is allowed when it helps teach the model:

- A profile is the saved result.
- A Git scope is the thing the user chooses while assembling that profile.
- `credential_ref` remains an implementation term. User-facing prompts should
  prefer **storage labels** and explain them in terms of the selected Git
  scope, reviewer entity, or LLM runtime.

## Core Mental Model

Interactive `cr init` should teach this composition:

`Git scope + reviewer entity + LLM runtime = review profile`

The user should understand:

- why they are configuring each building block
- which choices are reusable inside the current interactive session
- which settings are profile-local versus global

The user should not need to understand the on-disk config schema in order to
complete the primary path.

## GitHub Scope For V1

Prompt language for Git hosts should stay GitHub and GitHub Enterprise oriented
for v1.

- Active examples should use `github.com` and GitHub Enterprise hosts.
- The interactive flow should not imply that non-GitHub Git-host review is
  already supported end to end.
- Internal interfaces may remain host-agnostic without changing this prompt
  contract.

## Top-Level Interaction Model

Interactive `cr init` should behave like a workspace builder that lets the user
configure reusable building blocks first, then assemble one or more review
profiles before saving.

The intended top-level shape is:

1. Configure LLM runtimes
2. Configure reviewer entities
3. Configure review profiles
4. Configure global settings
5. Configure secrets management
6. Commit staged changes and exit
7. Discard staged changes and exit

This ordering is intentional:

- LLM runtime setup is usually obvious to the user and easy to reason about.
- Reviewer entity setup gives the user a clear explanation for who will post
  reviews and why they may need a separate actor.
- Review profiles compose those earlier choices into saved working
  configurations.
- Global settings stay visible but conceptually separate from the core
  identity/runtime/profile model.
- Secrets management stays top-level because it affects where credentials live,
  but it should not be confused with the credential refs inside a review
  profile.

## First-Run Behavior

For a first-time user, interactive `init` should:

- explain what `cr init` writes and what it does not write
- teach the three building blocks before asking for schema-shaped details
- avoid asking for raw credential refs on the primary path
- treat secrets as requirements of a building block, not as unrelated chores

The first-run user should be able to understand:

- what a profile is
- what Git scope means
- what a reviewer entity is
- why an LLM runtime is required
- what is optional versus required to make a profile usable

## Edit-Existing Behavior

Second-run interactive `init` is not a different product. It should preserve the
same mental model while respecting existing state.

When editing an existing profile, interactive `init` should:

- pre-populate the current selections
- default destructive changes to preserve
- surface missing required secrets early enough for the user to react before the
  final save
- keep route reconciliation tied to Git scope changes
- preserve the existing rename/default/route semantics owned by shared helpers

## Commit And Discard Semantics

Interactive `init` is draft-driven.

Until the user chooses **Commit staged changes and exit**:

- no config file writes occur
- no keyring writes occur
- prompts mutate only in-memory staged draft state
- route edits and reconciliations mutate only draft route state
- completing a subflow only stages changes in the current init session

Interactive `init` must offer both:

- **Commit staged changes and exit**: validate the draft, write any staged
  secret values, then write config and keyring state in the defined final
  commit order
- **Discard staged changes and exit**: discard the draft and leave both config and
  keyring untouched

Credential values may be collected inside the relevant subflow once the user has
enough local context to understand why each secret is needed. Reviewer-entity
setup collects the required PAT or GitHub App reviewer secrets inline on the
reviewer details page before a new reviewer can be staged. Those values remain
draft-local until commit; final commit still handles untouched or deferred Git
and LLM credential refs.

If the user cancels during credential collection, whether from a subflow or after
choosing **Commit staged changes and exit**, any pending secret values remain
draft-only and the session returns to a no-write state. Until final commit
begins, cancellation must still leave both config and keyring untouched.

Credential status shown inside a subflow must also be draft-driven. Reviewer
entity setup should show non-secret, per-key credential readiness for the
selected reviewer credential ref:

- PAT reviewers show `git_token`.
- GitHub App reviewers show required `github_app_id` and
  `github_app_private_key`, plus optional `github_app_installation_id`.
- Each key may be shown as `missing`, `existing`, `staged`, `skipped optional`,
  `deferred`, `optional`, or `status unavailable`.
- `missing` means the backend was consulted and no staged or existing value was
  found for a required key.
- `existing` means the backend reports a stored value for the key.
- `staged` means a draft-local value will be written at final commit.
- `skipped optional` means the user explicitly skipped an optional key in the
  current draft.
- `deferred` means the user deferred a required key in the current draft. New
  interactive reviewer setup should not offer deferral for required reviewer
  keys; they must be entered inline before staging.
- `optional` means an optional key has no staged, skipped, or existing value.
- `status unavailable` means the backend or key contract could not be inspected,
  so the UI must not claim a key is missing.
- The destination should include the storage label and the resolved
  secrets-management profile/backend when known.

This status must never show raw secret values. Draft-local writes, defers, and
optional-key skips should be preserved when the user re-enters reviewer entity
setup, but they must be filtered out if the reviewer credential ref no longer
uses those keys. Final commit remains the only write boundary for staged secret
values, and the final readiness summary should continue to report follow-up
credential work without leaking values.

All credential-bearing init flows should show equivalent non-secret destination
context before collecting secret values. This includes Git credentials, reviewer
PAT/GitHub App credentials, and LLM API keys handled by the shared credential
collector.

Destination summaries should include:

- credential storage ref
- resolved secrets-management profile label/id when a named profile applies
- resolved backend display label, including platform-specific automatic OS
  default copy such as `Automatic OS default (macOS Keychain)`
- configured 1Password vault, item title prefix, item tag, item field title, and
  other non-secret routing labels when present

Destination summaries must be non-fatal. If a profile/backend destination cannot
be resolved, the flow should show non-secret unavailable copy and continue to the
existing credential choice. They must never read or display backend token values;
environment variable names may be shown only as backend-auth env var names.

## Draft-Local Reuse Rules

LLM runtimes and reviewer entities are reusable **within the current interactive
`init` session only**.

That means:

- interactive draft state may name and reuse LLM runtimes across multiple draft
  profiles
- interactive draft state may name and reuse reviewer entities across multiple
  draft profiles
- no new persisted top-level runtime/entity schema is introduced in `config.yml`
- existing saved profiles are not auto-deduped into shared runtime/entity
  objects on reload

API-key-backed LLM runtimes may be reused across multiple profiles by selecting
the same draft runtime and the same advanced storage label. If a user wants two
distinct runtime entries that happen to use the same secret value, the system
should allow that and should not deduplicate them automatically.

## Reviewer Entity Contract

Interactive `init` should offer a user-facing reviewer entity fallback option named:

- **Use a profile's Git account (no separate reviewer entity)**

When the UI has additional profile context, it may render equivalent
contextual variants of the same fallback choice, such as:

- **Post as rianjs (GitHub PAT)**
- **Post as acme-review-bot (GitHub App)**
- **Post using this profile's Git account (GitHub PAT)**

This means:

- the review is posted with the profile's main Git credentials
- no separate reviewer credential ref is created for that choice
- in the current schema, that exports as `ReviewerCredentials=nil`

Interactive `init` may also offer separate reviewer entities such as:

- a personal access token (PAT) reviewer
- a GitHub App reviewer

For GitHub organizations, the UX should explain that a GitHub App is often the
preferred team/shared reviewer path.

Separate reviewer entities should also support an optional human-friendly
display name. When present, the configured reviewer-entity chooser should
prefer that display name and keep the technical reviewer type as supporting
text, for example:

- **OC Collective bot (GitHub App reviewer)**
- **Release reviewer (PAT reviewer)**

When no explicit display name exists, the chooser should fall back to stable,
deterministic identity text derived from the credential ref or equivalent
profile context, for example:

- **open-cli-collective-rianjs-bot (GitHub App reviewer)**
- **reviewer-pat (PAT reviewer)**

The profile-Git-account fallback is not a separately named reviewer entity and
should not ask for a custom reviewer-entity display name.

When the profile Git identity is known, fallback labels should prefer that
discovered identity plus the auth mode, for example `Post as rianjs (GitHub
PAT)`. When the identity is not yet known, the fallback should still make the
posting path explicit with `Post using this profile's Git account (<auth
mode>)`. This wording is derived from profile Git context such as
`git.identity_cache`; it does not create a separate reviewer entity label.

When multiple profiles already share the same separate reviewer identity, a
display-name edit in interactive `init` applies to that shared identity across
all of those profiles rather than only the active profile.

## User-Term To Schema Mapping

Interactive `init` may hide the raw schema, but the contract between user terms
and saved config must stay stable:

- **Git scope** maps to `profile.git`, plus repository-route implications when
  the profile participates in `repository_profiles`.
- **LLM runtime** maps to `profile.llm`, including the provider, auth mode,
  adapter, and any optional LLM credential ref implied by API-key auth.
- **Reviewer entity** maps to `profile.reviewer_credentials` when separate
  credentials are configured, or to `nil` when the user selected `Use a
  profile's Git account (no separate reviewer entity)`.
- **Review profile** maps to one saved entry under `profiles.<name>`.

This section is intentionally high level. The detailed field inventory and
mutation rules still live in `docs/init-config-surface.md`.

## Storage Labels

Interactive `init` should treat credential refs as an advanced concept without
forcing the user through a separate mode selector before they can see the
relevant values.

Primary-path users should not be asked for `credential_ref` values directly.
Instead:

- defaults should be generated automatically
- new reviewer defaults may follow the typed entity label, for example
  `rianjs-bot` becomes `codereview/rianjs-bot-reviewer`
- changing an existing reviewer label must not migrate or rename the existing
  credential-store ref
- users who need an override may edit the relevant inline **storage label**
  fields for Git, reviewer, or LLM secrets
- irrelevant reviewer/LLM storage-label fields should stay hidden when the
  profile is using its Git account or a subscription runtime
- any advanced path should still explain that these labels are non-secret
  pointers to keyring entries, not the secrets themselves
- users who need to change where secrets are stored should configure/select a
  secrets-management profile rather than entering GitHub App IDs, private keys,
  or API keys in the top-level secrets-management workflow

## Global Settings Area

Global settings belong in an optional top-level area, not in the main profile
assembly path.

The top-level auxiliary areas should cover:

- global settings for run-data retention behavior
- secrets-management settings for credential-store/backend behavior

These settings matter, but they are not part of the primary
`Git scope + reviewer entity + LLM runtime = review profile` model.

## Non-Interactive Regression Contract

The interactive workspace-builder work must not silently regress
`cr init --non-interactive`.

Follow-on tickets that touch shared `init` code should keep non-interactive
behavior readable and explicitly tested where relevant, especially for:

- `cr init --non-interactive` staying on its scripted path instead of routing
  through the interactive workspace draft model
- durable config ownership
- credential-planning behavior
- keyring backend persistence
- reviewer enable/disable behavior
- LLM auth and credential-ref behavior

## Relationship To The Config Surface Contract

Use this document for:

- interaction model
- user-facing terminology
- sequencing
- staging/commit semantics
- draft-local reuse rules

Use `docs/init-config-surface.md` for:

- durable config paths
- exact mutation semantics
- scripted ownership
- credential key matrices
- runtime-only flag boundaries
