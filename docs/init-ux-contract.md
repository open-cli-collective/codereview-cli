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
  prefer **advanced storage labels**.

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
4. Review global settings
5. Commit staged changes and exit
6. Discard staged changes and exit

This ordering is intentional:

- LLM runtime setup is usually obvious to the user and easy to reason about.
- Reviewer entity setup gives the user a clear explanation for who will post
  reviews and why they may need a separate actor.
- Review profiles compose those earlier choices into saved working
  configurations.
- Global settings stay visible but conceptually separate from the core
  identity/runtime/profile model.

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

- **Commit staged changes and exit**: validate the draft, collect or defer
  required secrets, then write config and keyring state in the defined
  final commit order
- **Discard staged changes and exit**: discard the draft and leave both config and
  keyring untouched

Credential collection belongs near final commit, after the user has assembled the
profile shape well enough to understand why each secret is needed.

If the user cancels during credential collection after choosing **Commit staged
changes and exit**, any pending secret values remain draft-only and the session
returns to a no-write state. Until final commit begins, cancellation must still
leave both config and keyring untouched.

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

## Advanced Storage Labels

Interactive `init` should treat credential refs as an advanced concept.

Primary-path users should not be asked for `credential_ref` values directly.
Instead:

- defaults should be generated automatically
- advanced users may inspect or override them through an **advanced storage
  labels** path
- any advanced path should still explain that these labels are non-secret
  pointers to keyring entries, not the secrets themselves

## Global Settings Area

Global settings belong in an optional top-level area, not in the main profile
assembly path.

The initial area should cover:

- keyring backend behavior
- run-data retention behavior

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
