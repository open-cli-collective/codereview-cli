# Scripts

Repository support and verification tools. These scripts are not part of the
shipped `cr` command surface.

macOS Keychain credential ACL repair is shared across the Open CLI Collective
tool family. Use the canonical helper from the sibling `cli-common` checkout:

```bash
../cli-common/scripts/repair-macos-keychain-credentials.sh --tool cr
```

Preview and apply the additive heal path:

```bash
../cli-common/scripts/repair-macos-keychain-credentials.sh --tool cr --heal
../cli-common/scripts/repair-macos-keychain-credentials.sh --tool cr --heal --apply
```

The heavier cleanup and rebuild paths also live in that shared helper.
