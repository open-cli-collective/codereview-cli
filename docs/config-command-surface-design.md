# Config Command Surface Design

This document defines the non-secret configuration command surface needed for
the current config true-up batch.

The goal is narrow: make already-supported non-secret config scriptable through
`cr` without requiring direct YAML edits. This batch does not redesign
bootstrap, secret ingress, or the full long-term profile management API.

## Scope

This document is the semantic parent for:

- `#167`: `cr config path` and default-profile commands
- `#166`: `cr config route` management commands
- `#169`: `cr config resolve-profile` route preview
- `#168`: `cr config agent-source` management commands
- `#294`: `cr config credential-store` inspection commands

Related but out of scope for this batch:

- `#164`: expand `init` to cover more scripted-install/bootstrap behavior
- future `cr config profile ...` command families
- any secret-ingress changes beyond existing `set-credential`

## Goals

- Expose existing non-secret config state through narrow `cr config` commands.
- Keep command semantics consistent across text output, JSON output,
  validation, idempotency, and profile selection.
- Preserve current runtime behavior for repository routing.
- Keep follow-on implementation tickets small and non-overlapping.

## Non-Goals

- Do not expand `cr init` in this batch.
- Do not add secret-writing behavior outside `cr set-credential`.
- Do not implement the full future profile mutation surface in this batch.
- Do not change repository-routing behavior as part of command-surface work
  unless an existing behavior bug is discovered.

## Current Reality

The codebase already supports:

- `repository_profiles` in config schema
- repository-aware profile resolution in runtime paths used by `cr review` and
  `cr agents`
- GitHub App reviewer auth

The missing piece is command coverage. Today `cr config` exposes `show`,
`clear`, and `llm models`, but path/default/route/route-preview/agent-source
operations still require manual YAML edits. Credential-store inspection is
available through `cr config credential-store`; creating, editing, and deleting
configured stores belongs to the interactive `cr init` secrets-storage flow.

## Shared Command Rules

These rules apply across the batch unless a ticket explicitly narrows them.

### Profile selection

- Commands that operate on a profile should use the existing global
  `--profile <name>` flag.
- When a command is not repository-aware, `--profile` selects the target
  profile and otherwise falls back to `default_profile`.
- Commands introduced in this batch must not invent a second profile-selection
  mechanism.

### Text and JSON output

- Every new read/inspect command should support `--json`.
- Text output should stay concise and script-friendly.
- JSON output should be explicit and stable enough for automation; it should
  prefer named fields over positional/implicit output.
- JSON shapes for related commands should use consistent field names where the
  underlying concepts are the same, such as `active_profile`, `profile`,
  `config_path`, `route`, and `source`.

### Validation and failure mode

- Commands should reuse existing config validation rules where possible instead
  of introducing looser parallel checks.
- Missing required arguments and malformed flag values are usage errors.
- References to nonexistent profiles are config/usage errors consistent with
  existing command behavior.
- Mutation commands must preserve unrelated config on success.

### Idempotency

- Mutation commands in this batch should be automation-friendly.
- `set`/`add` operations should converge on the requested state without
  creating duplicates.
- `unset`/`remove` operations should be safe to run repeatedly and succeed when
  the target state is already absent, unless a ticket explicitly justifies a
  stricter contract.

### Scope discipline

- Commands in this batch mutate only the config area named by their ticket.
- A command for one config concern should not opportunistically rewrite other
  config sections.

## Ticket Ownership Matrix

### `#167`: path and default profile

Owns:

- `cr config path [--json]`
- `cr config default get [--json]`
- `cr config default set <profile>`

Must not own:

- profile creation
- route mutation
- agent-source mutation
- `init` behavior changes

Required semantics:

- `default set` changes only `default_profile`
- `default set` validates that the target profile exists
- path inspection reports resolved config location only; broader state-path
  expansion is optional follow-up work, not required by this ticket

### `#166`: route mutation

Owns:

- `cr config route list [--json]`
- `cr config route set ...`
- `cr config route unset ...`

Must not own:

- route preview output beyond what is needed for route list/mutation
- runtime route-resolution redesign
- default-profile mutation

Required semantics:

- validate referenced profiles exist
- validate route host matches the target profile's `git.host`
- keep repo-specific and namespace-wide precedence unchanged
- normalize stored route values deterministically
- preserve unrelated routes and profiles

Route identity and mutation contract:

- a namespace-wide route is identified by `host + namespace` with omitted
  `repos`
- a repo-specific mapping is identified by `host + namespace + repo`
- `route set` with no `--repo` manages only the namespace-wide route for that
  `host + namespace`
- `route set` with one or more `--repo` values manages the listed repo-specific
  mappings for that `host + namespace`, assigning those repos to the requested
  profile
- if any listed repo is already mapped to another profile, `route set` moves
  that mapping to the requested profile and prunes it from the old repo-route
  entry as part of the same converging mutation
- repeated `--repo` inputs converge by trim, dedupe, and deterministic sort
- CLI-owned persistence should collapse repo-specific mappings into a
  deterministic shape: one repo-route entry per `profile + host + namespace`
  with sorted unique `repos`
- `route unset` with no `--repo` removes only the namespace-wide route for that
  `host + namespace`
- `route unset` with one or more `--repo` values removes only those
  repo-specific mappings; if the backing repo-route entry becomes empty, remove
  the route entry entirely

These semantics give `#166` a stable mutation contract without changing the
runtime precedence rules already used by review/agents.

### `#169`: route preview

Owns:

- `cr config resolve-profile <PR_URL> [--json]`

Must not own:

- independent route-matching logic
- live credential validation
- live identity refresh
- review-pipeline execution

Required semantics:

- reuse the same repository-resolution path already used by runtime commands
- report whether resolution came from explicit `--profile`, matched route, or
  fallback to `default_profile`
- remain a local preview command, not an execution path
- if the existing runtime resolution path does not expose enough metadata for
  preview output, `#169` may add a shared resolution-result wrapper/helper that
  preserves the same matching logic while surfacing source and matched-route
  details

The key contract is that `#169` reuses route-resolution semantics rather than
redefining them. If preview-specific formatting is needed, that formatting can
be unique, but the selection result must come from the same logic path as
review/agents.

### `#168`: agent-source mutation

Owns:

- `cr config agent-source list [--json]`
- `cr config agent-source add <path>`
- `cr config agent-source remove <path>`

Must not own:

- agent-source deployment or filesystem validation beyond normal config checks
- route behavior
- `init` expansion

Required semantics:

- operate on the selected profile only
- compare configured path strings in a predictable normalized form
- avoid filesystem canonicalization as mutation semantics
- preserve unrelated profile fields

Path normalization contract:

- normalize configured path strings by:
  - trimming surrounding whitespace
  - rejecting the value if it is empty after trim
  - applying `filepath.Clean(...)` to the remaining non-empty string
- do not expand `~`
- do not resolve symlinks
- do not case-fold paths
- do not require the target path to exist

This keeps mutation semantics deterministic and local-string-based rather than
filesystem-state-based.

### `#294`: credential-store inspection

Owns:

- `cr config credential-store list [--json]`
- `cr config credential-store get <id> [--json]`

Must not own:

- secret ingress or secret-value printing
- creating, editing, deleting, or selecting credential stores for a write
- any user-facing credential-store default

Required semantics:

- `list` reports the effective credential-store inventory, including the
  read-only built-in `local-os` store
- `get` reports one effective credential store by id
- configured stores are listed after `local-os` in stable order
- `local-os` is projected and is never persisted under `secrets.stores`
- JSON output must not include an `is_default` field for credential stores
- backend validation must reuse shared credstore metadata rather than duplicating backend-name knowledge in command code
- old `config secrets-profile ...` subcommands are rejected instead of acting as
  compatibility aliases

## `init` Boundary

`cr init` remains the existing bootstrap-oriented command in this batch.

This batch does not decide or implement a broader `init` expansion strategy.
That work belongs to `#164` and later follow-up design/implementation.

This boundary matters because scripted installs may want both:

- a friendly bootstrap command (`init`)
- narrow idempotent config mutation commands under `cr config`

This batch only defines the latter.

## Future Profile Management

The long-term `cr config profile ...` surface remains intentionally
non-binding in this batch.

This document may describe future design space, but future-oriented command
shapes are advisory only unless promoted into a dedicated implementation issue.
Follow-on tickets in this batch must not treat speculative future profile
commands as in scope.

The likely future design questions remain:

- whether profile mutation should be patch-like or declarative replacement
- whether field-specific subcommands and `profile set` should coexist
- how profile mutation should interact with credential locations and route safety

Those questions belong to later work after the current true-up lands.

## Acceptance Standard For The Batch

The batch is successful if:

- no supported non-secret config in these targeted areas requires manual YAML
  edits
- route preview and route mutation align with current runtime resolution
- the command surfaces are scriptable and idempotent
- `init` and secret-ingress scope remain unchanged

Anything beyond that is future work, not hidden scope.
