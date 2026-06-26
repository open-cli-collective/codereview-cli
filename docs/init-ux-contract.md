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

- **Repository access**: the reusable Git host and current-user Git credentials
  that define how `cr` accesses repositories. Examples for v1 are `github.com`
  and GitHub Enterprise hosts such as `github.mycompany.com`.
- **Reviewer entity**: the actor that posts `COMMENT`, `APPROVE`, or
  `REQUEST_CHANGES` on pull requests.
  On GitHub, a reviewer entity must resolve to a repository-authorized
  identity for `APPROVE` or `REQUEST_CHANGES` to count toward PR state. Live
  review stops before LLM or posting work when the selected reviewer can write
  a review object but GitHub would not treat it as an opinionated review.
- **LLM runtime**: the way reviewer agents run and authenticate, such as Claude
  CLI subscription auth, Codex CLI subscription auth, Pi local runtime, or a
  direct API-key-backed provider path.
- **Review profile**: one saved profile assembled from repository access, an
  LLM runtime, and a reviewer entity, plus its related review policy, routes,
  and optional advanced settings.

Supporting language is allowed when it helps teach the model:

- A profile is the saved result.
- Repository access is the thing the user chooses while assembling that profile.
- Credential storage is user-facing as **credential store** plus **credential
  name**. The name is the full visible `codereview/...` path written to the
  selected store.

## Core Mental Model

Interactive `cr init` should teach this composition:

`Repository access + reviewer entity + LLM runtime = review profile`

The user should understand:

- why they are configuring each building block
- which choices are reusable inside the current interactive session
- which settings are profile-local versus global

The user should not need to understand the on-disk config schema in order to
complete the primary path.

## GitHub Repository Access For V1

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

1. Configure secrets storage
2. Configure repository access
3. Configure LLM runtimes
4. Configure reviewer entities
5. Configure review profiles
6. Configure global settings
7. Commit staged changes and exit
8. Discard staged changes and exit

This ordering is intentional:

- LLM runtime setup is usually obvious to the user and easy to reason about.
- Repository access setup makes current-user Git host and credential routing a
  reusable building block instead of profile-local inline fields.
- Reviewer entity setup gives the user a clear explanation for who will post
  reviews and why they may need a separate actor.
- Review profiles compose those earlier choices into saved working
  configurations.
- Global settings stay visible but conceptually separate from the core
  identity/runtime/profile model.
- Secrets storage stays top-level because it affects where credentials live.
  Credential-writing flows choose one configured destination explicitly.

## First-Run Behavior

For a first-time user, interactive `init` should:

- explain what `cr init` writes and what it does not write
- teach the three building blocks before asking for schema-shaped details
- avoid asking for raw schema terms on the primary path
- treat secrets as requirements of a building block, not as unrelated chores

The first-run user should be able to understand:

- what a profile is
- what repository access means
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
- keep route reconciliation tied to repository access changes
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
setup collects the required PAT reviewer secret inline on the reviewer details
page before a new reviewer can be staged. Those values remain draft-local until
commit; final commit still handles untouched or deferred Git and LLM credential
locations.

If the user cancels during credential collection, whether from a subflow or after
choosing **Commit staged changes and exit**, any pending secret values remain
draft-only and the session returns to a no-write state. Until final commit
begins, cancellation must still leave both config and keyring untouched.

Credential status shown inside a subflow must also be draft-driven. Reviewer
entity setup should show non-secret, per-key credential readiness for the
selected reviewer credential location:

- PAT reviewers show `git_token`.
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
- The destination should include the credential store, credential name, and
  resolved backend when known.

This status must never show raw secret values. Draft-local writes, defers, and
optional-key skips should be preserved when the user re-enters reviewer entity
setup, but they must be filtered out if the reviewer credential location no
longer uses those keys. Final commit remains the only write boundary for staged
secret values, and the final readiness summary should continue to report
follow-up credential work without leaking values.

All credential-bearing init flows should show equivalent non-secret destination
context before collecting secret values. This includes repository-access Git
credentials, reviewer PAT/GitHub App credentials, and LLM API keys handled by
the shared credential collector.

Destination summaries should include:

- credential storage ref
- selected credential store id and display name
- credential name, including the visible `codereview/...` path
- resolved backend display label, including platform-specific OS store copy
  such as `macOS Login Keychain`
- configured 1Password account, vault name/id, and other non-secret routing
  metadata when present

Destination summaries must be non-fatal. If a profile/backend destination cannot
be resolved, the flow should show non-secret unavailable copy and continue to the
existing credential choice. They must never read or display backend token values;
environment variable names may be shown only as backend-auth env var names.

## Draft-Local Reuse Rules

Repository access definitions, LLM runtimes, and reviewer entities are reusable
saved building blocks.

That means:

- interactive draft state may name and reuse repository access across multiple
  draft profiles
- interactive draft state may name and reuse LLM runtimes across multiple draft
  profiles
- interactive draft state may name and reuse reviewer entities across multiple
  draft profiles
- committed repository access definitions are persisted under
  `repository_access`
- committed LLM runtimes are persisted under `llm_runtimes`
- committed reviewer entities are persisted under `reviewer_entities`
- saved profiles reference those building blocks by name

API-key-backed LLM runtimes may be reused across multiple profiles by selecting
the same draft runtime and the same credential store/name. If a user wants two
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

- the review is posted with the profile's selected repository access credentials
- no separate reviewer credential location is created for that choice
- in the current schema, that exports as `ReviewerCredentials=nil`

Interactive `init` may also offer separate reviewer entities such as:

- a personal access token (PAT) reviewer

For GitHub organizations, the UX should explain that reviewer identities must
use PAT auth when the review needs GitHub to count `APPROVE` or
`REQUEST_CHANGES` toward PR state.

Separate reviewer entities should also support an optional human-friendly
display name. When present, the configured reviewer-entity chooser should
prefer that display name and keep the technical reviewer type as supporting
text, for example:

- **Release reviewer (PAT reviewer)**

When no explicit display name exists, the chooser should fall back to stable,
deterministic identity text derived from the credential name or equivalent
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

- **Repository access** maps to a saved `repository_access.<name>` entry. A
  profile selects it with `profiles.<profile>.repository_access`, and runtime
  code resolves `profile.git` from that selection.
- **LLM runtime** maps to a saved `llm_runtimes.<name>` entry. A profile selects
  it with `profiles.<profile>.llm_runtime`.
- **Reviewer entity** maps to a saved `reviewer_entities.<name>` entry. A
  profile selects it with `profiles.<profile>.reviewer.kind: entity` and
  `profiles.<profile>.reviewer.entity`. The Git-account fallback maps to
  `profiles.<profile>.reviewer.kind: git_identity`.
  A GitHub reviewer entity is not just posting credentials: the resolved
  identity must also have repository authority for GitHub to count blocking or
  approving reviews toward the PR decision.
- **Review profile** maps to one saved entry under `profiles.<name>`.

This section is intentionally high level. The detailed field inventory and
mutation rules still live in `docs/init-config-surface.md`.

## Credential Storage

Interactive `init` should show credential storage as an explicit destination
choice, not as a hidden default.

Primary-path users should choose a credential store and see or edit the full
credential name when configuring the building block that owns that credential.
There is no namespace, label, or prefix prompt. Instead:

- defaults should be generated automatically
- repository access owns current-user Git credential locations
- new reviewer defaults may follow the typed entity label, for example
  `rianjs-bot` becomes `codereview/rianjs-bot-reviewer`
- changing an existing reviewer display name must not migrate or rename the
  existing credential name
- users who need an override may edit the relevant **credential name** field on
  the repository-access, reviewer-entity, or LLM-runtime building block
- irrelevant reviewer/LLM credential-name fields should stay hidden when a
  reviewer entity uses no separate secret or a runtime uses subscription auth
- any advanced path should still explain that these names are non-secret
  pointers to credential-store entries, not the secrets themselves
- users who need to change where secrets are stored should configure/select a
  credential store rather than entering GitHub App IDs, private keys, or API
  keys in the top-level secrets-storage workflow

## Global Settings Area

Global settings belong in an optional top-level area, not in the main profile
assembly path.

The top-level auxiliary areas should cover:

- global settings for run-data retention behavior
- secrets-storage settings for credential-store/backend behavior

These settings matter, but they are not part of the primary
`Repository access + reviewer entity + LLM runtime = review profile` model.

## Non-Interactive Regression Contract

The interactive workspace-builder work must not silently regress
`cr init --non-interactive`.

Follow-on tickets that touch shared `init` code should keep non-interactive
behavior readable and explicitly tested where relevant, especially for:

- `cr init --non-interactive` staying on its scripted path instead of routing
  through the interactive workspace draft model
- durable config ownership
- credential-planning behavior
- explicit credential-store destination behavior
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
