# Scripts

This directory contains repository support and verification tools. These scripts
are not part of the shipped `cr` command surface.

## `repair-codereview-keychain-acl.sh`

Repairs macOS Keychain generic-password ACLs for the `codereview` service when
older items still trust only ad-hoc or per-build `cdhash` identities instead of
the stable-signed `cr` release binary.

Use it when `cr` repeatedly prompts for the same Keychain items after upgrading
from older unsigned or ad-hoc-signed builds. Run it as the normal macOS user who
owns the login Keychain, not with `sudo`.

Default mode is a dry-run:

```bash
./scripts/repair-codereview-keychain-acl.sh
```

Apply repairs explicitly:

```bash
./scripts/repair-codereview-keychain-acl.sh --apply
```

The script does not read or print secret values. In `--apply` mode it updates
only `cdhash-only` generic-password items for the selected service and keychain
so they trust the current stable-signed `cr` binary. Items classified as
`missing-current-cr` are reported for manual inspection rather than changed.

Apply mode is intentionally additive. For explicit app-list ACL entries, the
helper appends current `cr` trust while preserving existing trusted applications.
It skips decrypt ACLs with `NULL` or non-explicit app lists instead of narrowing
their meaning to one trusted application.

`stable+stale-cdhash` means current `cr` is already trusted. macOS may still
report older cdhash grants or partition metadata for that item; the script treats
that state as repaired and does not try to purge historical metadata.
