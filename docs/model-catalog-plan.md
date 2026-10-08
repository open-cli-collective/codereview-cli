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
