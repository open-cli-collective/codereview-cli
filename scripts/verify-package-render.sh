#!/usr/bin/env bash
set -euo pipefail

dist_dir="${1:-dist}"

fail() {
  echo "package render check failed: $*" >&2
  exit 1
}

require_file() {
  [ -f "$1" ] || fail "missing file: $1"
}

require_grep() {
  local pattern="$1" file="$2"
  grep -Fq "$pattern" "$file" || fail "$file missing: $pattern"
}

require_file "$dist_dir/metadata.json"
require_file "$dist_dir/artifacts.json"
version="$(jq -r '.version' "$dist_dir/metadata.json")"
[ -n "$version" ] && [ "$version" != "null" ] || fail "metadata version missing"

for arch in amd64 arm64; do
  require_file "$dist_dir/cr_v${version}_windows_${arch}.zip"
  require_file "$dist_dir/cr_${version}_linux_${arch}.deb"
  require_file "$dist_dir/cr_${version}_linux_${arch}.rpm"
done

cask="$dist_dir/homebrew/Casks/codereview-cli.rb"
require_file "$cask"
require_grep 'cask "codereview-cli"' "$cask"
require_grep 'binary "cr"' "$cask"
require_grep 'open-cli-collective/codereview-cli/releases/download/v' "$cask"
require_grep 'cr_v#{version}_darwin_arm64.tar.gz' "$cask"
require_grep 'cr_v#{version}_darwin_amd64.tar.gz' "$cask"

for kind in deb rpm; do
  for arch in amd64 arm64; do
    jq -e --arg kind ".$kind" --arg arch "$arch" '
      .[] | select(.type == "Linux Package" and .goarch == $arch and .extra.Ext == $kind and .extra.ID == "cr")
    ' "$dist_dir/artifacts.json" >/dev/null || fail "missing cr linux package artifact: $kind/$arch"
  done
done

winget_installer="packaging/winget/OpenCLICollective.cr.installer.yaml"
require_file "packaging/winget/OpenCLICollective.cr.yaml"
require_file "$winget_installer"
require_file "packaging/winget/OpenCLICollective.cr.locale.en-US.yaml"
require_grep "PackageIdentifier: OpenCLICollective.cr" "$winget_installer"
require_grep "PortableCommandAlias: cr" "$winget_installer"
require_grep "cr_v0.0.0_windows_amd64.zip" "$winget_installer"
require_grep "cr_v0.0.0_windows_arm64.zip" "$winget_installer"

require_file "packaging/chocolatey/cr.nuspec"
require_file "packaging/chocolatey/tools/chocolateyInstall.ps1"
require_grep "<id>cr</id>" "packaging/chocolatey/cr.nuspec"
require_grep 'releases/download/v${version}' "packaging/chocolatey/tools/chocolateyInstall.ps1"
require_grep 'cr_v${version}_windows_${arch}.zip' "packaging/chocolatey/tools/chocolateyInstall.ps1"

require_grep 'homebrew-tap-token: ${{ secrets.HOMEBREW_TAP_TOKEN }}' ".github/workflows/release.yml"
require_grep 'chocolatey-api-key: ${{ secrets.CHOCOLATEY_API_KEY }}' ".github/workflows/release.yml"
require_grep 'winget-token: ${{ secrets.WINGET_GITHUB_TOKEN }}' ".github/workflows/release.yml"
require_grep 'linux-dispatch-token: ${{ secrets.LINUX_PACKAGES_DISPATCH_TOKEN }}' ".github/workflows/release.yml"

echo "package render check OK"
