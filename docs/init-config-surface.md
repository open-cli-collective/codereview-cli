# Init Configuration Surface

This document is the design contract for expanding `cr init` under #164. It
binds the follow-up child issues #176 through #187: later implementation plans
should either follow the ownership and semantics below or explicitly amend this
document first.

Product-facing interaction and terminology for the interactive experience now
live in [docs/init-ux-contract.md](init-ux-contract.md).
Use that document for the workspace-builder model, user-facing terms, and
staging/commit semantics. Use this document for durable config ownership, mutation
rules, and scripted/non-interactive boundaries.

`cr init` must configure durable, non-secret `config.yml` state and credential
references. It must not turn one-shot runtime flags into durable settings, and
it must not write secret values to config, logs, stdout, stderr, or errors.

## Action Vocabulary

- **Skip**: the user does not enter a section. Existing values are preserved;
  missing values remain missing unless required to build a valid new profile.
- **Preserve**: keep the current value exactly, including custom credential
  refs and identity cache values.
- **Overwrite**: replace the field with a new value after validation.
- **Clear/reset**: remove the configured value so validation defaults or
  built-in behavior apply. This is available only where the schema supports an
  omitted value.
- **Defer**: store a valid non-secret reference but do not collect the matching
  secret yet. The command must print or render follow-up credential commands.

Second-run interactive init defaults to **preserve** for every existing value.
Optional sections must be skippable. Destructive actions such as clear/reset,
credential-ref overwrite, and secret overwrite must be explicit.

## Durable Config Inventory

Each durable config path appears once in this table. The scripted owner column
is the final non-interactive ownership decision for #187: existing init flag,
future #187 init flag, existing config command, #186 docs sequence, or
intentionally unsupported.

| Path | Interactive handling | Scripted owner | Defaults and second-run prepopulation | Mutation semantics | Helper/command owner and expected tests |
|------|----------------------|----------------|---------------------------------------|--------------------|-----------------------------------------|
| config.secrets.stores | Configure secrets storage creates and edits user-configured credential stores. The built-in OS credential store is projected as `local-os` and is not persisted here. | Interactive main-menu secrets-storage flow only. Scripted credential writes choose an existing store explicitly. | Omitted means only the built-in OS credential store is available. Existing stores are pre-populated in the inventory. | Store ids must not be `local-os`. Store metadata is non-secret. Creating, editing, and deleting configured stores never deletes credential values. | #356 establishes explicit stores and removes ambient/default credential-store selection. |
| config.llm_runtimes | Configure LLM runtimes creates and edits reusable runtime definitions independently from review profiles. Profiles select one configured runtime when composing a reviewer. | Interactive main-menu LLM runtime flow only for now. Future config commands may own scripted runtime CRUD. | Omitted means no reusable runtimes are configured yet. The profile editor can send the user to the runtime flow when no runtime exists. Existing runtimes are pre-populated in the inventory. | Runtime names are stable ids. Subscription runtimes store only non-secret provider/auth/adapter settings. API-key runtimes store an explicit credential location and never the secret value. Deleting a runtime requires affected profiles to select a replacement or be edited. | #356 makes runtime setup standalone and keeps profile composition explicit. |
| config.repository_profiles[] | Route editor opens directly with existing namespace and repo routes prefilled so the user can edit them in place or blank the field to remove all routes for the profile. | Existing `cr config route set` and `cr config route unset`; #186 documents route commands. #187 must not add a nested multi-route init grammar. | Empty or omitted means no automatic repository profile selection; runtime commands require `--profile` until a matching route is configured. Existing routes are prefilled on later runs. | Leaving the prefilled text unchanged preserves routes. Mutations must converge through shared route helpers and preserve unrelated routes. Blanking the field removes the profile's routes. | #177 extracts route helpers. #185 tests list/edit/remove, route canonicalization, and preservation. |
| config.repository_profiles[].profile | Route wizard selects the profile that a route resolves to. | Existing `cr config route set --profile <name> ...`. | No default inside an entry; required when a route exists. Existing profile is pre-populated. | Overwrite only to an existing profile. Profile rename updates matching route entries. | #177 profile rename/route reference helper. #185 tests rename updates and invalid profile rejection. |
| config.repository_profiles[].match.host | Route wizard derives from selected profile host or pasted PR URL. | Existing `cr config route set --host`. | Required for a route. Existing host is pre-populated and normalized. | Must match the target profile's `git.host`. Host edits on a profile with routes are blocked or reconciled by #185. | #177 route helper, #185 PR URL derivation and host/profile validation tests. |
| config.repository_profiles[].match.namespace | Route wizard asks for owner/org or derives from PR URL. | Existing `cr config route set --namespace`. | Required for a route. Existing namespace is pre-populated. | Overwrite validates non-empty and preserves repo-vs-namespace route identity. | #177 route helper. #185 namespace route tests. |
| config.repository_profiles[].match.repos[] | Route wizard supports namespace-wide route when omitted or repo-specific routes when provided. | Existing repeatable `cr config route set --repo` and `cr config route unset --repo`. | Omitted means namespace-wide route. Existing repos are pre-populated in deterministic order. | Preserve on skip. Add/edit/remove dedupes and sorts via shared route helper. Clear repos converts only when the user explicitly chooses namespace-wide routing. | #177 route helper. #185 repo route and namespace conversion tests. |
| config.profiles.<name> | Core profile wizard selects existing profile, creates a new profile, or renames an existing profile. | Existing global `--profile` with `cr init --non-interactive` owns scripted create/replace. Scripted rename is intentionally unsupported by init and belongs to future profile-management command design. | New profile names start blank. Existing profile names are pre-populated. | Create requires a valid profile body. Rename preserves credential locations by default, updates repository routes, and does not delete old credential-store entries. | #177 profile rename helper. #180 tests create/select/rename and validation. |
| config.profiles.<name>.git.host | Core profile wizard edits Git host. | Existing `cr init --git-host`; existing route commands own route-safe scripted changes. | Current init defaults to `github.com`. Existing value is pre-populated. | Set validates non-empty normalized host. If routes reference the profile and reconciliation is not selected, block or defer the edit. | #180 blocks/defer route-unsafe host edits. #185 implements reconciliation tests. |
| config.profiles.<name>.git.auth_mode | Core profile wizard chooses `pat` or `github_app`; `oauth_device` remains reserved. | `cr init --git-auth-mode`, parallel to existing reviewer auth mode. Current init otherwise defaults user Git auth to PAT. | Existing value is pre-populated. New profile defaults to `pat`. | Overwrite only to supported v1 modes. Switching auth modes re-plans credential keys but does not delete old secrets. | #179 credential-ref/key-spec planner. #180 auth-mode prompt tests. |
| config.profiles.<name>.git.credential.store | User Git auth chooses an existing credential store before any secret ingress. | Interactive credential-writing flows and scripted `cr set-credential --store`. | New profiles start with `local-os` selected. Existing store id is pre-populated. | Must be `local-os` or a configured store id. Store setup is not available inline from this flow. | #356 explicit destination tests. |
| config.profiles.<name>.git.credential.name | User Git auth names the credential bundle before any secret ingress. | Interactive credential-writing flows and scripted `cr set-credential --name`. | New profile defaults to `codereview/<profile>`. Existing name is pre-populated and preserved by default. | Must use the `codereview/<profile>` credential grammar. It must differ from other credentials in the same profile when the store also matches. | #356 explicit credential-location tests. |
| config.profiles.<name>.git.identity_cache | Not shown as an editable init field. Preserve only. | Intentionally unsupported for init and config mutation. Runtime identity refresh owns it. | Existing cache is preserved. New profiles omit it. | Preserve unless a future explicit cache invalidation ticket owns behavior. Profile rename does not rewrite cache contents. | Preserve-only regression in #177/#180. |
| config.reviewer_entities | Configure reviewer entities creates and edits reusable posting identities independently from review profiles. Entity definitions include host, auth mode, credential location, display name, and identity cache. | Interactive main-menu reviewer-entity flow only for now. Future config commands may own scripted reviewer-entity CRUD. | Omitted means no separate reviewer identities are configured yet. Profiles can still choose `git_identity`. Existing entities are pre-populated in the inventory. | Entity names are stable ids. PAT and GitHub App entities store explicit credential locations and never the secret values. Deleting a referenced entity requires affected profiles to select a replacement or use Git identity. | First-class reviewer-entity data model tests and init reviewer-entity UX tests. |
| config.profiles.<name>.reviewer.kind | Review profile composition chooses whether posting uses the profile Git account or a configured reviewer entity. | Interactive review-profile flow only for now. | New profiles require an explicit choice. Existing value is pre-populated. | `git_identity` requires empty `reviewer.entity`; `entity` requires an existing reviewer entity whose host matches profile Git host. | First-class reviewer-reference validation tests. |
| config.profiles.<name>.reviewer.entity | Review profile composition selects one configured reviewer entity when `reviewer.kind` is `entity`. | Interactive review-profile flow only for now. | Empty for `git_identity`. Existing entity reference is pre-populated. | Must reference `config.reviewer_entities`. The entity credential must differ from Git and selected LLM runtime credentials when the store also matches. | First-class reviewer-reference validation tests. |
| config.profiles.<name>.llm_runtime | Review profile composition selects one configured LLM runtime by name. | Interactive review-profile flow only for now. Future config commands may own scripted profile composition. | New profiles require an explicit runtime choice. Existing runtime reference is pre-populated. | Must reference `config.llm_runtimes`. API-key runtime credentials must differ from Git and selected reviewer entity credentials when the store also matches. | First-class LLM-runtime reference validation tests. |
| config.profiles.<name>.agent_sources[] | Direct trusted-directory editor shows existing profile agent-source paths in one multiline field with explanatory notes about trust and separate repo-local `.codereview/agents` discovery. | Existing `cr init --agent-source`; existing `cr config agent-source add/remove`; #186 docs sequence. | Omitted means no additional profile-specific trusted directories. Existing values are pre-populated line-by-line. | Preserve on skip. Clearing the field removes all profile-specific agent sources. Entered paths are normalized and deduped through the shared config-edit helper. | #177 extracts helper. #183 tests add/remove/reset/preserve. #289 prompt tests cover explanatory copy, multiline entry, and Back. |
| config.profiles.<name>.review_policy.major_event | Review-policy wizard chooses comment or request_changes. | Existing `cr init --major-event`. | Current init defaults to `comment`. Existing value is pre-populated. | Preserve on skip. Set validates enum. Reset clears to normalized default `comment`. | #183 tests prompt, reset, and validation. |
| config.profiles.<name>.review_policy.allow_self_approve | Review-policy wizard toggles durable self-approval policy. | Existing `cr init --allow-self-approve`. | Current init defaults false. Existing value is pre-populated. | Preserve on skip. Set true/false explicitly. Runtime `cr review --allow-self-approve` remains separate. | #183 tests true/false preservation. |
| config.profiles.<name>.review_policy.resolve_threads | Review-policy wizard chooses unset, auto, or never. | Existing `cr init --resolve-threads`. | Empty means normal runtime behavior. Existing value is pre-populated. | Preserve on skip. Reset clears field. Set validates enum. Runtime `--no-resolve-threads` remains one-shot. | #183 tests unset/auto/never. |
| config.profiles.<name>.review_policy.resolve_after | Review-policy wizard edits or clears the configured duration. | Existing `cr init --resolve-after`. | Empty means no configured delay. Existing value is pre-populated. | Preserve on skip. Reset clears field. Set validates Go duration. | #183 tests duration validation and clear. |
| config.data.retention.max_age_days | Global retention editor shows one direct `Maximum run-data age in days` field plus explanatory run-data copy. | New `cr config retention set/reset` from #178. #187 must not add retention init flags without amending this contract. | Omitted normalizes to 90. Explicit `0` means keep forever. Existing value is pre-populated with the effective current value, including `0` for keep forever. | Preserve on skip. Blank input resets to the 90-day default. Set accepts non-negative days. `0` keeps posted-review run data indefinitely. | #178 command tests and #184 wizard tests cover omitted/default vs explicit 0. #290 prompt tests cover direct prefills, blank-to-default reset, and Back. |
| config.data.retention.enforcement | Not shown in interactive init; retained as command-level/power-user config. | New `cr config retention set/reset` from #178. #187 must not add retention init flags without amending this contract. | Omitted normalizes to `at_write`. Existing value is preserved when init edits retention. | Interactive init preserves the current enforcement value. `cr config retention` remains the path for explicit `at_write` vs `manual_only` changes. | #178 command tests and #184 wizard tests cover reset/manual-only. #290 init tests cover preservation when editing max age. |

## Profile Lifecycle Semantics

Each credential-writing workflow stores an explicit credential location in the
profile: `credential.store` plus `credential.name`. The built-in OS credential
store is always addressable as `local-os`, and user-configured stores live under
`secrets.stores`. Runtime code must use the credential location attached to the
Git, reviewer, or LLM credential being read or written; it must not infer a
destination from a profile-level or global credential-store default.

The same `credential.name` may appear in different stores. Within one profile,
credential locations must not collide when both the store and name match.

Profile selection and mutation is shared by #177 and consumed by #180:

- Creating a profile builds a complete, valid profile before saving.
- Selecting an existing profile pre-populates every editable value.
- Renaming a profile updates `config.repository_profiles[].profile` entries
  that point at the old name.
- Renaming a profile preserves all existing credential locations by default,
  even names that look auto-generated. This avoids stranding existing
  credential-store entries.
- A wizard may offer credential-name regeneration only when #181 has explicit
  migration or overwrite behavior for the affected credential-store entries.
- Rename never deletes old credential-store entries implicitly.
- Rename preserves `git.identity_cache` and
  `reviewer_credentials.identity_cache`. Cache invalidation requires a future
  dedicated owner.

## Git Host And Route Safety

`repository_profiles[].match.host` must match the referenced profile's
`git.host`. Because of that validation rule, editing `git.host` can invalidate
routes.

Before #185 landed, the core wizard blocked or deferred a host edit when routes
existed for the profile. Interactive init now owns the reconciliation flow:

- show impacted routes before applying the host change
- update route hosts when the user chooses to reconcile
- preserve unrelated routes
- reject route hosts that still do not match the selected profile
- use the same route helper as `cr config route`

The route editor in interactive init accepts one route per line in any of these
forms:

- `host/namespace`
- `host/namespace/repo`
- `host/namespace [repo1, repo2]`
- `https://github.com/<owner>/<repo>/pull/<number>`

Pasted GitHub PR URLs derive `match.host`, `match.namespace`, and the repo name
from the URL and save the route as a repo-specific route for the selected
profile.

## Credential Bundle Matrix

Credential locations are non-secret config: each write has an explicit store and
name. Secret values are written only through the existing credential store
plumbing. #179 owns credential-name/key-spec planning; #181 owns interactive
secret ingress.

| Purpose | Auth/provider | Name default | Required keys | Optional keys | Keep/defer/overwrite semantics | Migration rule |
|---------|---------------|-------------|---------------|---------------|--------------------------------|----------------|
| User Git auth | `pat` | `codereview/<profile>` | `git_token` | None | Keep preserves existing store/name and key. Defer stores the credential location and prints follow-up `cr set-credential --store ... --name ...`. Overwrite writes `git_token` through the selected credential store only. | Profile rename preserves the credential location. Regenerate only with explicit key migration or overwrite. |
| Reviewer Git auth | `pat` | `codereview/<profile>-reviewer`, or label-derived for new interactive reviewer entities | `git_token` | None | Interactive reviewer setup collects `git_token` inline before staging a new or changed reviewer credential location. Keep preserves an unchanged credential location. Clearing the reviewer section removes the location from config but does not delete secrets. | Profile rename and reviewer label edits preserve existing credential locations. No implicit rename/migration. |
| User or reviewer Git auth | `github_app` | Same purpose-specific defaults as PAT | `github_app_id`, `github_app_private_key` | `github_app_installation_id` | Keep preserves bundle. Interactive reviewer setup collects required GitHub App keys inline before staging a new or changed reviewer credential location; optional installation ID may be left blank. Scripted/deferred flows print one follow-up command per required key. Overwrite writes only keys the user provided, with required-key validation before saving. | Migration is bundle-wide and explicit only: never leave config pointing at a partially moved bundle. |
| Git auth | `oauth_device` | None | None | None | Unsupported in v1. The wizard must not offer it as a selectable mode. | Future OAuth work must amend this document. |
| LLM API key | `anthropic` + `api_key` | `codereview/<profile>-llm` | `anthropic_api_key` | None | Keep preserves existing credential location. Defer stores the location only if the key already exists or follow-up is clearly rendered. Overwrite writes provider key through the selected credential store. | Preserve custom locations on rename. Regeneration requires explicit migration/overwrite. |
| LLM API key | `openai` + `api_key` | `codereview/<profile>-llm` | `openai_api_key` | None | Same as Anthropic API-key auth. | Same as Anthropic API-key auth. |
| LLM adapter-managed auth | `subscription` | No credential location | None | None | Keep/preserve leaves `llm.credential` empty. Switching from API key to subscription clears `llm.credential` only after confirmation. | No credential-store migration because config no longer points at a location. |
| LLM Pi auth | `pi` + `subscription` + `pi_rpc` | No credential location | None | None | Supported adapter-managed mode. | No credential-store migration. |
| LLM Pi API key | `pi` + `api_key` | None | None | None | Unsupported in v1. The wizard must not offer this combination. | Future Pi API-key support must amend this document. |

Secret ingress rules:

- Only one stdin-backed secret source can consume stdin in a single command.
- Interactive secret entry may read a no-echo paste or clipboard value, but the
  value must be injected behind test fakes.
- Empty secret input is an error when a secret source was selected.
- `--overwrite` and interactive overwrite replace existing keyring entries only
  after preflight confirms the target bundle can be written.
- If config save fails after keyring writes, errors must identify refs needing
  cleanup without printing secret values.

## Retention Semantics

Retention is global config under `data.retention`, not profile config.

- Omitted `max_age_days` normalizes to 90 days.
- Explicit `max_age_days: 0` means keep forever and must survive round trips.
- Negative `max_age_days` is invalid.
- Omitted `enforcement` normalizes to `at_write`.
- `manual_only` disables review-time pruning and leaves `cr data prune` as the
  explicit maintenance path.
- Reset means restore default behavior: 90 days and `at_write`. Because current
  config save normalizes retention before encoding, reset may write explicit
  default values unless #178 changes save/normalization behavior to preserve
  omission. Reset must never encode `max_age_days: 0`.

#178 must add retention command tests for omitted/default vs explicit zero.
#184 must use the same validation and reset behavior in interactive init.

## Scripted Install Ownership

Scripted installs should remain readable. The intended shape is:

1. Run `cr init --non-interactive` for the profile's core non-secret config and
   any secrets intentionally supplied through stdin/env ingress.
2. Use `cr set-credential` for scripted, deferred, or post-init credential
   bundles.
3. Use `cr config route`, `cr config agent-source`, `cr config llm models`,
   and `cr config retention` for narrow idempotent
   mutations.

Credential storage is configured from the interactive main menu under
**Configure secrets storage**. Scripted credential writes must choose a
destination explicitly with `cr set-credential --store <id> --name <name>`.
The root `--backend` flag remains runtime-only compatibility behavior and must
not become durable config.

#187 adds only the durable init flags assigned to it in the inventory table:
`--git-auth-mode`, `--disable-reviewer`, `--llm-reviewer-model-tier`,
and `--clear-llm-reviewer-model-tier`.
It must not add a nested multi-route grammar, model-map flags, literal secret
flags, or hidden YAML-in-a-flag structures.

## Runtime-only Flag Audit

The following flags are intentionally not durable init configuration.

| Command | Flags | Rationale |
|---------|-------|-----------|
| Global/root | `--version` | Process output only. |
| Global/root | `--backend` | Compatibility runtime credential backend selector. It cannot override explicit credential-store destinations and must not persist to config. |
| Global/root | `--profile` | Runtime profile selector. It participates in init profile selection but is not itself a durable field. |
| `cr review` execution mode | `--dry-run`, `--no-post`, `--rerun`, `--retry-posts` | Per-run execution behavior, not profile policy. |
| `cr review` output/audit | `--json`, `--verbose` | Presentation and diagnostic controls. |
| `cr review` PR/run targeting | `--review-base-sha`, `--review-head-sha`, `--session` | Per-run targeting or session reuse. |
| `cr review` local resources | `--agents-dir`, `--max-agents`, `--max-concurrency` | Per-run resource and test controls. Durable trusted sources use `agent_sources`. |
| `cr review` dry-run model overrides | `--selection-model`, `--selection-effort`, `--selection-prompt`, `--reviewer-model`, `--reviewer-model-tier`, `--reviewer-effort` | Dry-run override surface for experiments. Durable reviewer baseline is `llm.reviewer_model_tier`; durable tier-to-model mapping is `llm.model_map`. |
| `cr review` posting gates | `--fail-on`, `--allow-self-review`, `--allow-self-approve`, `--no-resolve-threads` | One-shot live review gates. Durable self-approval and thread policy are `review_policy.allow_self_approve` and `review_policy.resolve_threads`. |
| `cr init` control | `--non-interactive`, `--replace-profile` | Command flow controls. They select scripted mode and replacement behavior but do not create durable config fields. |
| `cr init` secret ingress | `--git-token-stdin`, `--git-token-from-env`, `--reviewer-token-stdin`, `--reviewer-token-from-env`, `--llm-api-key-stdin`, `--llm-api-key-from-env`, `--overwrite` | Secret ingress and overwrite controls. They may be part of init interaction, but their values must never become config. |
| `cr set-credential` | `--store`, `--name`, `--key`, `--stdin`, `--from-env`, `--overwrite`, `--json` | Credential-store operation, not config schema. |
| `cr config` read/output | `--json`, `--dry-run` where present | Presentation or preview-only behavior. |
| `cr config route` | `--host`, `--namespace`, `--repo` | Command arguments for route mutation. The durable result is `repository_profiles`. |
| `cr config llm models reset` | `--provider` | Safety guard for reset, not stored config. |
| `cr config clear` | `--all`, `--dry-run`, `--json` | Cleanup command behavior, not config. |
| `cr data prune` | `--dry-run`, `--older-than`, `--keep-last`, `--json` | Manual maintenance operation. Durable automatic retention uses `data.retention`. |
| `cr data purge` | `--dry-run`, `--yes`, `--json` | Manual destructive operation. |
| `cr data show` | `--json` | Presentation only. |
| `cr agents` | `--agents-dir`, `--json` | Per-run inspection inputs and output. Durable trusted sources use `agent_sources`. |
| `cr me` | `--all`, `--json` | Identity refresh/read behavior, not wizard configuration. |
| `cr sessions` | `--json` | Presentation only. |
| `cr benchmark` | `--candidate`, `--case`, `--results-dir`, `--cr-bin`, `--json` | Benchmark harness controls, outside init. |

## Follow-up Issue Ownership

- #176: init plan/apply architecture with no behavior change.
- #177: shared mutation helpers for profile rename, routes, agent sources,
  retention validation, and optional clear/reset behavior.
- #178: `cr config retention` command surface.
- #179: credential-location and key-spec planning.
- #180: non-secret core interactive wizard.
- #181: interactive secret ingress and safe keyring writes.
- #182: interactive `llm.model_map`.
- #183: interactive `agent_sources` and `review_policy`.
- #184: interactive global `data.retention` and secrets-storage settings.
- #185: interactive repository routes and host reconciliation.
- #186: scripted installer documentation.
- #187: maintainable non-interactive init parity flags.
