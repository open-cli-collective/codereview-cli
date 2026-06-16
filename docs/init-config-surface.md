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
| config.default_profile | Core profile wizard lets the user preserve, set to the selected profile, or choose another existing profile. | Existing `cr config default set`; `cr init --set-default` owns the scripted "make this profile default" case during init. | First init sets the created profile as default. Existing value is pre-populated and preserved by default. | Overwrite only to an existing profile. Profile rename updates this field when it points at the renamed profile. | #177 shared profile rename/default helper. #180 tests create, preserve, set, and rename updates. |
| config.keyring.backend | Secrets-management wizard currently offers preserve, set, or reset to auto/env/default backend resolution as a legacy persisted backend preference until named secrets-management profiles replace it. | `cr init --keyring-backend` and `--reset-keyring-backend` are the explicit durable init surface. Existing global `--backend` remains the runtime selector, though legacy write-backed init paths may still persist it for compatibility. | Empty means backend auto/env selection. Existing value is pre-populated. | Preserve by default. Set validates through credential backend parsing. Reset clears the field. | Existing keyring validation plus #184/#187 tests cover set, reset, backend conflict, legacy runtime persistence, and unrelated config preservation. |
| config.secrets.default_profile | Not yet editable in interactive init. This ticket sequence introduces named secrets-management lego bricks first; later tickets let review profiles select them. | `cr config secrets-profile default get/set/unset`; later init/profile selection tickets. Not owned by legacy `keyring.backend`. | Empty means there is no configured default named secrets-management profile. Legacy configs instead project a read-only `legacy-default` compatibility profile in presentation/helpers. | Overwrite only to an existing named secrets-management profile id. `legacy-default` is reserved/projected and cannot be set. Mixed configs with both `keyring.backend` and `secrets.*` keep legacy runtime backend behavior until the later resolver ticket changes credential selection. | #293 establishes schema, validation, and compatibility projection. #294 adds scriptable default get/set/unset without changing runtime routing. Later secrets-profile selection tickets own mutation UX. |
| config.secrets.profiles.<name>.label | Not yet editable in interactive init. Used for human-readable secrets-management profile naming in later tickets. | `cr config secrets-profile set --label/--clear-label`; later init/profile management tickets. | Optional. Empty means UI surfaces may fall back to the stable profile id. Existing value is pre-populated when later editors arrive. | Preserve on skip. Overwrite trims whitespace. Must remain a single line. Whitespace-only labels are rejected. Duplicate labels are allowed; ids remain authoritative. | #293 establishes schema/validation plus safe display summaries. #294 adds scriptable create/update/clear-label behavior. |
| config.secrets.profiles.<name>.backend.kind | Not yet editable in interactive init. Durable backend choice for a named secrets-management profile. | `cr config secrets-profile set --backend`; later init/profile management tickets. | Required for an explicit named secrets-management profile. Legacy configs with no explicit `secrets.profiles` continue to project `legacy-default` from `keyring.backend` or auto/env resolution without persisting a synthetic entry. | Preserve on skip. Overwrite validates against recognized credential-store backend names. This ticket does not yet change runtime credential routing; it only establishes the durable model and summaries. | #293 establishes schema/validation, projected legacy inventory, and config-show summaries. #294 adds scriptable create/update/remove/default management. Later resolver/backend tickets consume it. |
| config.profiles.<name>.secrets_profile | Not yet editable in interactive init; later tickets add a selector that chooses an existing secrets-management profile for the review profile. | Manual YAML today; later review-profile selection tickets own explicit mutation UX. Not owned by legacy `keyring.backend`. | Empty means the review profile follows legacy/default runtime resolution: use `secrets.default_profile` when configured, otherwise fall back to legacy `keyring.backend`/env/auto behavior. Existing explicit values are preserved. | Overwrite only to an existing named secrets-management profile id. `legacy-default` is reserved/projected and must not be persisted directly. Clearing the field restores legacy/default runtime resolution rather than forcing a named profile. | #295 introduces the schema plus runtime credential routing through this field. #298 owns the user-facing selection workflow and recovery UX for missing/deleted references. |

Runtime credential resolution for `profiles.<name>.secrets_profile` now follows
this order:

1. the active review profile's explicit `secrets_profile`
2. `secrets.default_profile`, when configured
3. legacy backend fallback via `keyring.backend`, environment selection, or
   auto detection

Shared credential refs use the same resolved store only when every declaring
profile converges on the same logical secrets-management profile. When they do
not:

- `cr` with `--profile <name>` uses that selected profile only when it owns the
  ref, and errors when another profile owns the ref instead
- `cr` without `--profile` fails fast and tells the user to pass `--profile` to
  disambiguate the conflicting owners
| config.repository_profiles[] | Route editor opens directly with existing namespace and repo routes prefilled so the user can edit them in place or blank the field to remove all routes for the profile. | Existing `cr config route set` and `cr config route unset`; #186 documents route commands. #187 must not add a nested multi-route init grammar. | Empty or omitted means default-profile fallback. Existing routes are prefilled on later runs. | Leaving the prefilled text unchanged preserves routes. Mutations must converge through shared route helpers and preserve unrelated routes. Blanking the field removes the profile's routes. | #177 extracts route helpers. #185 tests list/edit/remove, route canonicalization, and preservation. |
| config.repository_profiles[].profile | Route wizard selects the profile that a route resolves to. | Existing `cr config route set --profile <name> ...`. | No default inside an entry; required when a route exists. Existing profile is pre-populated. | Overwrite only to an existing profile. Profile rename updates matching route entries. | #177 profile rename/route reference helper. #185 tests rename updates and invalid profile rejection. |
| config.repository_profiles[].match.host | Route wizard derives from selected profile host or pasted PR URL. | Existing `cr config route set --host`. | Required for a route. Existing host is pre-populated and normalized. | Must match the target profile's `git.host`. Host edits on a profile with routes are blocked or reconciled by #185. | #177 route helper, #185 PR URL derivation and host/profile validation tests. |
| config.repository_profiles[].match.namespace | Route wizard asks for owner/org or derives from PR URL. | Existing `cr config route set --namespace`. | Required for a route. Existing namespace is pre-populated. | Overwrite validates non-empty and preserves repo-vs-namespace route identity. | #177 route helper. #185 namespace route tests. |
| config.repository_profiles[].match.repos[] | Route wizard supports namespace-wide route when omitted or repo-specific routes when provided. | Existing repeatable `cr config route set --repo` and `cr config route unset --repo`. | Omitted means namespace-wide route. Existing repos are pre-populated in deterministic order. | Preserve on skip. Add/edit/remove dedupes and sorts via shared route helper. Clear repos converts only when the user explicitly chooses namespace-wide routing. | #177 route helper. #185 repo route and namespace conversion tests. |
| config.profiles.<name> | Core profile wizard selects existing profile, creates a new profile, or renames an existing profile. | Existing global `--profile` with `cr init --non-interactive` owns scripted create/replace. Scripted rename is intentionally unsupported by init and belongs to future profile-management command design. | Empty global `--profile` means `default` during current init. Existing profile names are pre-populated. | Create requires a valid profile body. Rename preserves credential refs by default, updates default_profile and routes, and does not delete old keyring entries. | #177 profile rename helper. #180 tests create/select/rename and validation. |
| config.profiles.<name>.git.host | Core profile wizard edits Git host. | Existing `cr init --git-host`; existing route commands own route-safe scripted changes. | Current init defaults to `github.com`. Existing value is pre-populated. | Set validates non-empty normalized host. If routes reference the profile and reconciliation is not selected, block or defer the edit. | #180 blocks/defer route-unsafe host edits. #185 implements reconciliation tests. |
| config.profiles.<name>.git.auth_mode | Core profile wizard chooses `pat` or `github_app`; `oauth_device` remains reserved. | `cr init --git-auth-mode`, parallel to existing reviewer auth mode. Current init otherwise defaults user Git auth to PAT. | Existing value is pre-populated. New profile defaults to `pat`. | Overwrite only to supported v1 modes. Switching auth modes re-plans credential keys but does not delete old secrets. | #179 credential-ref/key-spec planner. #180 auth-mode prompt tests. |
| config.profiles.<name>.git.credential_ref | Credential-ref planner chooses ref for user Git auth before any secret ingress. | Existing `cr init --git-credential-ref`; `cr set-credential` writes secrets. | New profile defaults to `codereview/<profile>`. Existing ref is pre-populated and preserved by default. | Flattened interactive init shows the effective Git storage label inline. Leaving that field at the effective default means "follow the selected Git scope's default label" across later scope changes; entering a different value makes it a preserved custom override. Clear is invalid because Git credentials are required. | #179 planner tests default/custom refs and required state. #181 secret ingress tests. |
| config.profiles.<name>.git.identity_cache | Not shown as an editable init field. Preserve only. | Intentionally unsupported for init and config mutation. Runtime identity refresh owns it. | Existing cache is preserved. New profiles omit it. | Preserve unless a future explicit cache invalidation ticket owns behavior. Profile rename does not rewrite cache contents. | Preserve-only regression in #177/#180. |
| config.profiles.<name>.reviewer_credentials | Optional reviewer credential section. Wizard supports skip, preserve, enable, edit, or clear reviewer config. | Existing `cr init --reviewer-credential-ref` and `--reviewer-auth-mode` own enable/edit. `cr init --disable-reviewer` owns scripted removal. | Omitted means posting uses Git credentials. Existing section is pre-populated. | Clear removes the whole section. Enable requires auth mode and credential ref. Ref must differ from Git and LLM refs. | #179 planner and #180 optional section tests. #181 secret ingress tests. |
| config.profiles.<name>.reviewer_credentials.auth_mode | Reviewer wizard chooses PAT or GitHub App; `oauth_device` remains reserved. | Existing `cr init --reviewer-auth-mode`. | Current init defaults reviewer mode to `pat` when reviewer credentials are requested. Existing value is pre-populated. | Overwrite only to supported v1 modes. Switching modes re-plans key specs and preserves old secrets unless explicit overwrite/migration occurs. | #179 credential bundle tests for PAT and GitHub App. |
| config.profiles.<name>.reviewer_credentials.credential_ref | Credential-ref planner chooses reviewer Git ref. | Existing `cr init --reviewer-credential-ref`; `cr set-credential` writes secrets. | New reviewer section defaults to `codereview/<profile>-reviewer`. Existing ref is pre-populated and preserved. | Flattened interactive init shows the effective reviewer storage label only when a separate reviewer entity is active. Leaving the field at its effective default means "follow the selected reviewer entity's default label" across later entity changes; entering a different value makes it a preserved custom override. Switching back to posting with the profile Git account clears the reviewer ref from config. | #179 planner tests collision handling. #181 secret ingress tests. |
| config.profiles.<name>.reviewer_credentials.display_name | Reviewer wizard lets the user set or clear a human-friendly label for a separate reviewer entity. | Interactive `cr init` only for now. No dedicated scripted owner yet. | Empty means chooser labels fall back to deterministic credential-ref/profile-derived text. Existing value is pre-populated. | Preserve on skip. Overwrite trims surrounding whitespace. Clear is valid and returns the entity to fallback labeling. Conflicting labels across profiles that share one reviewer identity do not create duplicate identities; the shared chooser entry falls back to deterministic identity text until one label wins. | #244 tests round-trip, validation, shared-identity conflict fallback, and chooser labeling. |
| config.profiles.<name>.reviewer_credentials.identity_cache | Not shown as an editable init field. Preserve only. | Intentionally unsupported for init and config mutation. Runtime identity refresh owns it. | Existing cache is preserved. New reviewer sections omit it. | Preserve unless a future explicit cache invalidation ticket owns behavior. | Preserve-only regression in #177/#180. |
| config.profiles.<name>.llm.provider | Core profile wizard chooses provider. | Existing `cr init --llm-provider`. | Current init defaults to `anthropic`. Existing value is pre-populated. | Overwrite validates provider and may require compatible auth/adapter/key specs. | #179 provider credential planning. #180 provider/auth/adapter compatibility tests. |
| config.profiles.<name>.llm.auth | Core profile wizard chooses subscription or API key auth. | Existing `cr init --llm-auth`. | Current init defaults to `subscription`. Existing value is pre-populated. | Subscription requires empty `llm.credential_ref`; API key requires a provider-supported ref and secret plan. | #179 planner tests subscription/API-key transitions. #181 ingress tests. |
| config.profiles.<name>.llm.adapter | Core profile wizard chooses compatible adapter. | Existing `cr init --llm-adapter`. | Current init defaults to `claude_cli`. Existing value is pre-populated. | Overwrite validates provider/auth compatibility, including `openai+codex_cli+subscription` and `pi+pi_rpc+subscription`. | #180 compatibility tests. |
| config.profiles.<name>.llm.credential_ref | Credential-ref planner chooses LLM API-key ref when required. | Existing `cr init --llm-credential-ref`; `cr set-credential` writes secrets. | Omitted for subscription auth. API-key auth defaults to `codereview/<profile>-llm`. Existing ref is pre-populated. | Flattened interactive init shows the effective LLM storage label only for API-key runtimes. Leaving the field at its effective default means "follow the selected runtime's default API-key label" across later runtime changes; entering a different value makes it a preserved custom override. Switching back to subscription clears the LLM ref from config. Ref must differ from Git/reviewer refs. | #179 planner tests. #181 API-key ingress/no-leak tests. |
| config.profiles.<name>.llm.model_map | Model-tier mapping editor opens directly with the effective tier mappings for the selected runtime. Built-in mappings are shown in-place, configured overrides are shown in-place, and unmapped tiers stay visibly empty. | Existing `cr config llm models set/unset/reset`; #186 documents this scripted sequence. #187 must not add model-map init flags. | Omitted uses provider/adapter built-ins. Existing overrides are merged into the visible effective values. | Leaving a built-in value unchanged keeps the built-in mapping without serializing a redundant override. Entering a different value stores an explicit override. Clearing a prefilled override falls back to the built-in mapping when one exists, otherwise leaves the tier unmapped. | #182 tests add/edit/remove/reset and invalid entries, plus prompt-level round trips for keeping and clearing prefilled override-backed tiers. |
| config.profiles.<name>.llm.reviewer_model_tier | Core profile wizard frames this as a minimum reviewer model tier and offers the built-in small baseline or explicit small, medium, and large baselines. | `cr init --llm-reviewer-model-tier` and `--clear-llm-reviewer-model-tier`, because no config command currently mutates this field directly. | Omitted means effective small baseline. Existing value is pre-populated. | Preserve on skip. Reset clears field. Set validates model tier. | #180 prompt tests. #187 flag persistence tests. |
| config.profiles.<name>.agent_sources[] | Direct trusted-directory editor shows existing profile agent-source paths in one multiline field with explanatory notes about trust and separate repo-local `.codereview/agents` discovery. | Existing `cr init --agent-source`; existing `cr config agent-source add/remove`; #186 docs sequence. | Omitted means no additional profile-specific trusted directories. Existing values are pre-populated line-by-line. | Preserve on skip. Clearing the field removes all profile-specific agent sources. Entered paths are normalized and deduped through the shared config-edit helper. | #177 extracts helper. #183 tests add/remove/reset/preserve. #289 prompt tests cover explanatory copy, multiline entry, and Back. |
| config.profiles.<name>.review_policy.major_event | Review-policy wizard chooses comment or request_changes. | Existing `cr init --major-event`. | Current init defaults to `comment`. Existing value is pre-populated. | Preserve on skip. Set validates enum. Reset clears to normalized default `comment`. | #183 tests prompt, reset, and validation. |
| config.profiles.<name>.review_policy.allow_self_approve | Review-policy wizard toggles durable self-approval policy. | Existing `cr init --allow-self-approve`. | Current init defaults false. Existing value is pre-populated. | Preserve on skip. Set true/false explicitly. Runtime `cr review --allow-self-approve` remains separate. | #183 tests true/false preservation. |
| config.profiles.<name>.review_policy.resolve_threads | Review-policy wizard chooses unset, auto, or never. | Existing `cr init --resolve-threads`. | Empty means normal runtime behavior. Existing value is pre-populated. | Preserve on skip. Reset clears field. Set validates enum. Runtime `--no-resolve-threads` remains one-shot. | #183 tests unset/auto/never. |
| config.profiles.<name>.review_policy.resolve_after | Review-policy wizard edits or clears the configured duration. | Existing `cr init --resolve-after`. | Empty means no configured delay. Existing value is pre-populated. | Preserve on skip. Reset clears field. Set validates Go duration. | #183 tests duration validation and clear. |
| config.data.retention.max_age_days | Global retention editor shows one direct `Maximum run-data age in days` field plus explanatory run-data copy. | New `cr config retention set/reset` from #178. #187 must not add retention init flags without amending this contract. | Omitted normalizes to 90. Explicit `0` means keep forever. Existing value is pre-populated with the effective current value, including `0` for keep forever. | Preserve on skip. Blank input resets to the 90-day default. Set accepts non-negative days. `0` keeps posted-review run data indefinitely. | #178 command tests and #184 wizard tests cover omitted/default vs explicit 0. #290 prompt tests cover direct prefills, blank-to-default reset, and Back. |
| config.data.retention.enforcement | Not shown in interactive init; retained as command-level/power-user config. | New `cr config retention set/reset` from #178. #187 must not add retention init flags without amending this contract. | Omitted normalizes to `at_write`. Existing value is preserved when init edits retention. | Interactive init preserves the current enforcement value. `cr config retention` remains the path for explicit `at_write` vs `manual_only` changes. | #178 command tests and #184 wizard tests cover reset/manual-only. #290 init tests cover preservation when editing max age. |

## Profile Lifecycle Semantics

Profile selection and mutation is shared by #177 and consumed by #180:

- Creating a profile builds a complete, valid profile before saving.
- Selecting an existing profile pre-populates every editable value.
- Renaming a profile updates `config.default_profile` when it points at the old
  name.
- Renaming a profile updates `config.repository_profiles[].profile` entries
  that point at the old name.
- Renaming a profile preserves all existing credential refs by default, even
  refs that look auto-generated. This avoids stranding existing keyring
  entries.
- A wizard may offer credential-ref regeneration only when #181 has explicit
  migration or overwrite behavior for the affected keyring entries.
- Rename never deletes old keyring profiles implicitly.
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

Credential refs are non-secret config. Secret values are written only through
the existing credential store plumbing. #179 owns ref/key-spec planning; #181
owns interactive secret ingress.

| Purpose | Auth/provider | Ref default | Required keys | Optional keys | Keep/defer/overwrite semantics | Migration rule |
|---------|---------------|-------------|---------------|---------------|--------------------------------|----------------|
| User Git auth | `pat` | `codereview/<profile>` | `git_token` | None | Keep preserves existing ref and key. Defer stores ref and prints follow-up `cr set-credential`. Overwrite writes `git_token` through keyring only. | Profile rename preserves ref. Regenerate only with explicit key migration or overwrite. |
| Reviewer Git auth | `pat` | `codereview/<profile>-reviewer` | `git_token` | None | Same as user Git, but ref must differ from user Git ref. Clearing the reviewer section removes the ref from config but does not delete secrets. | Profile rename preserves ref unless #181 migrates or overwrites. |
| User or reviewer Git auth | `github_app` | Same purpose-specific defaults as PAT | `github_app_id`, `github_app_private_key` | `github_app_installation_id` | Keep preserves bundle. Defer stores ref and prints one follow-up command per key. Overwrite writes only keys the user provided, with required-key validation before saving. | Migration is bundle-wide: never leave config pointing at a partially moved bundle. |
| Git auth | `oauth_device` | None | None | None | Unsupported in v1. The wizard must not offer it as a selectable mode. | Future OAuth work must amend this document. |
| LLM API key | `anthropic` + `api_key` | `codereview/<profile>-llm` | `anthropic_api_key` | None | Keep preserves existing ref. Defer stores ref only if the key already exists or follow-up is clearly rendered. Overwrite writes provider key through keyring. | Preserve custom refs on rename. Regeneration requires explicit migration/overwrite. |
| LLM API key | `openai` + `api_key` | `codereview/<profile>-llm` | `openai_api_key` | None | Same as Anthropic API-key auth. | Same as Anthropic API-key auth. |
| LLM adapter-managed auth | `subscription` | No ref | None | None | Keep/preserve leaves empty ref. Switching from API key to subscription clears `llm.credential_ref` only after confirmation. | No keyring migration because config no longer points at a ref. |
| LLM Pi auth | `pi` + `subscription` + `pi_rpc` | No ref | None | None | Supported adapter-managed mode. | No keyring migration. |
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
2. Use `cr set-credential` for deferred or multi-key credential bundles.
3. Use `cr config default`, `cr config route`, `cr config agent-source`,
   `cr config llm models`, and `cr config retention` for narrow idempotent
   mutations.

Persistent `keyring.backend` selection now belongs to `cr init
--non-interactive` through `--keyring-backend` and
`--reset-keyring-backend`. There is still no standalone `cr config
keyring ...` command. For backward compatibility, init may also persist a
runtime `--backend` when the command writes credentials or configures API-key
LLM auth; that older path remains supported but is no longer the recommended
scripted contract.

#187 adds only the durable init flags assigned to it in the inventory table:
`--git-auth-mode`, `--keyring-backend`, `--reset-keyring-backend`,
`--disable-reviewer`, `--llm-reviewer-model-tier`,
`--clear-llm-reviewer-model-tier`, and `--set-default`.
It must not add a nested multi-route grammar, model-map flags, literal secret
flags, or hidden YAML-in-a-flag structures.

## Runtime-only Flag Audit

The following flags are intentionally not durable init configuration.

| Command | Flags | Rationale |
|---------|-------|-----------|
| Global/root | `--version` | Process output only. |
| Global/root | `--backend` | Runtime credential backend selector. It may persist `keyring.backend` only through explicit init/config backend semantics; most invocations remain one-shot. |
| Global/root | `--profile` | Runtime profile selector. It participates in init profile selection but is not itself a durable field. |
| `cr review` execution mode | `--dry-run`, `--no-post`, `--rerun`, `--retry-posts` | Per-run execution behavior, not profile policy. |
| `cr review` output/audit | `--json`, `--verbose` | Presentation and diagnostic controls. |
| `cr review` PR/run targeting | `--review-base-sha`, `--review-head-sha`, `--session` | Per-run targeting or session reuse. |
| `cr review` local resources | `--agents-dir`, `--max-agents`, `--max-concurrency` | Per-run resource and test controls. Durable trusted sources use `agent_sources`. |
| `cr review` dry-run model overrides | `--selection-model`, `--selection-effort`, `--selection-prompt`, `--reviewer-model`, `--reviewer-model-tier`, `--reviewer-effort` | Dry-run override surface for experiments. Durable reviewer baseline is `llm.reviewer_model_tier`; durable tier-to-model mapping is `llm.model_map`. |
| `cr review` posting gates | `--fail-on`, `--allow-self-review`, `--allow-self-approve`, `--no-resolve-threads` | One-shot live review gates. Durable self-approval and thread policy are `review_policy.allow_self_approve` and `review_policy.resolve_threads`. |
| `cr init` control | `--non-interactive`, `--replace-profile` | Command flow controls. They select scripted mode and replacement behavior but do not create durable config fields. |
| `cr init` secret ingress | `--git-token-stdin`, `--git-token-from-env`, `--reviewer-token-stdin`, `--reviewer-token-from-env`, `--llm-api-key-stdin`, `--llm-api-key-from-env`, `--overwrite` | Secret ingress and overwrite controls. They may be part of init interaction, but their values must never become config. |
| `cr set-credential` | `--ref`, `--key`, `--stdin`, `--from-env`, `--overwrite`, `--json` | Credential-store operation, not config schema. |
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
- #179: credential-ref and key-spec planning.
- #180: non-secret core interactive wizard.
- #181: interactive secret ingress and safe keyring writes.
- #182: interactive `llm.model_map`.
- #183: interactive `agent_sources` and `review_policy`.
- #184: interactive global `data.retention` and legacy secrets-management `keyring.backend`.
- #185: interactive repository routes and host reconciliation.
- #186: scripted installer documentation.
- #187: maintainable non-interactive init parity flags.
