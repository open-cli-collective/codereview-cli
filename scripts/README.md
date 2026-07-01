# Scripts

Repository support and verification tools. These scripts are not part of the
shipped `cr` command surface.

macOS Keychain credential ACL repair is shared across the Open CLI Collective
tool family. The canonical helper lives in
`open-cli-collective/cli-common`:

- Source of truth:
  <https://github.com/open-cli-collective/cli-common/blob/main/scripts/repair-macos-keychain-credentials.sh>
- If you already have `../cli-common` checked out locally, you can run it from
  the sibling checkout:

```bash
../cli-common/scripts/repair-macos-keychain-credentials.sh --tool cr
```

Preview and apply the additive heal path:

```bash
../cli-common/scripts/repair-macos-keychain-credentials.sh --tool cr --heal
../cli-common/scripts/repair-macos-keychain-credentials.sh --tool cr --heal --apply
```

The heavier cleanup and rebuild paths also live in that shared helper.
