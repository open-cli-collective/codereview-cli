# Model catalog delivery plan

The model catalog is an immutable, validated snapshot loaded once per command.
The binary embeds an offline baseline; an installed snapshot or explicit local
directory may replace it. Runtime identity and defaults live in `runtimes.csv`,
`models.csv`, and `defaults.csv`; price observations live in `pricing.csv`.
`manifest.json` records the schema and revision. Go retains adapter harness,
authentication, and transport behavior.

`cr catalog update` validates a fetched snapshot completely before atomically
installing it. `cr catalog show` reports its source, revision, runtime/model
capabilities, and pricing coverage. Review commands never fetch a catalog.
User model and effort maps retain precedence over catalog defaults. Unknown
explicit models remain usable at standard speed; fast and ultrafast are enabled
only when the selected runtime/model row says so and the adapter maps that
transport. Ultrafast rows record published rates where verified, while this
release adds no selector until an adapter maps that mode. Pricing remains
unavailable when the catalog lacks a verified rate.

Verification sources and dates are recorded in the CSV rows and manifest notes.

## Acceptance checks

The implementation is verified with the repository test suites plus direct CLI
checks against temporary catalogs: a clean local update changes the active
revision, a malformed or checksum-mismatched update leaves the previous
pointer active, and an HTTP update rejects a manifest without checksums. The
same binary is exercised with the bundled snapshot, an installed snapshot,
and an explicit `--catalog` path. `catalog show --json` is checked for source,
revision, capability, and pricing metadata; review fast selection is checked
for selected resolved models while an unselected unsupported offer remains
usable at standard speed. The rendered package hook is checked for a bounded,
warning-only refresh. The live locally built `--fast` review is the current PR
delivery gate; its final result will be recorded after independent review.

## Executed evidence

The focused catalog, configuration, adapter, pipeline, pricing, command, and
resume tests pass. They include explicit-catalog snapshot caching, all
capability states and invalid values, known-model effort restrictions alongside
unknown explicit models, selected-catalog pricing basis, and fresh or corrupt
installed-pointer handling. `golangci-lint` reports no issues, `make build` passes, and
`make snapshot` completes its GoReleaser build and package render check. The
generated cask passes Ruby syntax validation. A built local binary was run in
an isolated home directory: `catalog show --json` read the bundled baseline,
a local directory update installed a new revision, a duplicate-model update
failed while retaining that revision, and a loopback HTTP update installed a
later revision. The rendered postflight body was exercised through both its
success and warning-only failure branches; the Homebrew implementation keeps
the refresh bounded by the catalog updater's 30-second context.

The repository-wide test target has intermittent process-group and timing
failures in existing subprocess, RPC, hook, and lock tests when run in
parallel; the affected packages pass when rerun serially, and the snapshot
build's own test hook passed. OpenAI context-banded estimates remain unknown
when usage metadata cannot identify the applicable band; the published rows
are still visible through `catalog show`. Fast and ultrafast rows describe
verified capabilities, while only adapter mappings can consume them.

## Request-pricing schema and activation gate

Catalog schema 2 appends five pricing CSV columns, in order: `runtime_id`,
`observed_service_tiers`, `input_tokens_min_inclusive`,
`input_tokens_max_exclusive`, and `rate_application`. The schema-1 reader keeps
its original exact 11-column header and produces no request bindings. Schema 2
requires the exact extended header, with no leading, trailing or quoted name
whitespace. Schema-1 historical header/cell handling is preserved. Data cells
keep their established whitespace trimming in either schema; header whitespace
must never erase populated bindings or bounds. Old binaries reject schema 2; coordinate
published update-catalog rollout with binary rollout. A failed explicit update
keeps the previously installed snapshot and does not silently fall back.

All five metadata cells blank or null means legacy/display-only. A complete
binding requires a supported runtime, exact observed tier spellings, a
nonnegative decimal int64 minimum, and `whole_request`; an absent maximum means
unbounded only within that complete binding. The short and long rows must form
matching `[0,B)` and `[B,unbounded)` intervals, or an all row must span
`[0,unbounded)`. Every supported raw-tier identity must be unambiguous. Bounds
and rates are row data, including for explicit custom catalogs; the loader does
not impose a provider-wide threshold or rate multiplier.

The reviewed initial bindings cover 24 existing rows for GPT-6.1 Sol, GPT-6
Astra/Sol/Luna, and GPT-5.6 Sol/Terra/Luna. Their short band includes 272000
inclusive input tokens and their long band starts at 272001. Rates are consumed
literally from the selected row. Standard binds only observed `default`, Fast
binds observed `fast` or `priority`, and Astra Ultrafast binds `ultrafast`.
Unrecognized, omitted, requested-only or differently capitalized tiers do not
match. GPT-5.4/5.5 remain display-only pending clarification of their documented
full-session billing unit. Claude all-context rows remain legacy records.

`Catalog.PriceForRequest` uses the exact observed model and raw service tier,
runtime, and one request's inclusive input count. It returns a detached row and
catalog schema/revision/digest, never an aggregate estimate. `Catalog.Digest`
is the full SHA-256 of the exact five-file snapshot, including local snapshots
without manifest checksums; the human revision alone is not sufficient identity.
All price accessors deep-copy bindings, tiers, numeric bounds and rate pointers.
`catalog show --json` exposes those binding semantics and the full digest.

No production estimator is activated by metadata. Legacy aggregate Usage cannot
consume request-bound rows even when `context_band=all`. A later request-cost
integration must independently establish official global OpenAI API endpoint
scope, known input/cache/output categories, every dispatch (including retries),
complete history and a persisted evidence/catalog basis. Custom gateways,
regional endpoints, subscription runtimes and aggregate-only old records cannot
inherit these bindings. Selection does not validate model context capacity or
include non-token tool charges. A future result is a public list-price token
estimate, not an invoice total.

Source and test changes for this schema are subject to separate runtime
verification; the earlier executed evidence above does not cover this slice.
