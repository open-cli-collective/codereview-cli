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

require_absent() {
  [ ! -e "$1" ] || fail "stale file present: $1"
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
ruby -c "$cask" >/dev/null || fail "invalid rendered Homebrew cask Ruby"
require_grep 'cask "codereview-cli"' "$cask"
require_grep 'binary "cr"' "$cask"
require_grep 'open-cli-collective/codereview-cli/releases/download/v' "$cask"
require_grep 'cr_v#{version}_darwin_arm64.tar.gz' "$cask"
require_grep 'cr_v#{version}_darwin_amd64.tar.gz' "$cask"
require_grep '"com.apple.quarantine", "{{staged_path}}/cr"' "$cask"

# GoReleaser renders the install_steps hook as Homebrew's install-step DSL,
# which has no system_command: any statement other than a run step makes
# Homebrew reject the whole cask as invalid. A legacy postflight block means the
# hook moved back to GoReleaser's deprecated install key, which fails
# `goreleaser check` in the release workflow.
ruby - "$cask" <<'RUBY'
source = File.read(ARGV.fetch(0))
raise "legacy postflight block present" if source.match?(/^  postflight do$/)

body = source[/  postflight_steps do\n(.*?)\n  end/m, 1]
raise "postflight_steps body missing" unless body

body.each_line do |line|
  next if line.strip.empty?
  next if line.match?(/\A\s*run\s/)

  raise "postflight_steps line is not a run step: #{line.strip}"
end
puts "rendered package hook check OK"
RUBY

for kind in deb rpm pkg.tar.zst; do
  for arch in amd64 arm64; do
    jq -e --arg kind "$kind" --arg dotted ".$kind" --arg arch "$arch" '
      .[] | select(
        .type == "Linux Package" and
        .goarch == $arch and
        (.extra.Ext == $kind or .extra.Ext == $dotted) and
        .extra.ID == "cr"
      )
    ' "$dist_dir/artifacts.json" >/dev/null || fail "missing cr linux package artifact: $kind/$arch"
  done
done

# Winget and Chocolatey keep version/checksum placeholders in source; the shared
# release workflow substitutes them from the real release assets before publish.
# The generated windows archives above prove the archive naming those templates
# reference is rendered by GoReleaser.
require_grep "winget: { id: OpenCLICollective.codereview-cli, bootstrap: true }" "packaging/identity.yml"
require_grep "chocolatey: { id: codereview-cli }" "packaging/identity.yml"

require_absent "packaging/winget/OpenCLICollective.cr.yaml"
require_absent "packaging/winget/OpenCLICollective.cr.installer.yaml"
require_absent "packaging/winget/OpenCLICollective.cr.locale.en-US.yaml"
require_absent "packaging/chocolatey/cr.nuspec"

winget_version="packaging/winget/OpenCLICollective.codereview-cli.yaml"
winget_installer="packaging/winget/OpenCLICollective.codereview-cli.installer.yaml"
winget_locale="packaging/winget/OpenCLICollective.codereview-cli.locale.en-US.yaml"
require_file "$winget_version"
require_file "$winget_installer"
require_file "$winget_locale"
require_grep "PackageIdentifier: OpenCLICollective.codereview-cli" "$winget_version"
require_grep "PackageIdentifier: OpenCLICollective.codereview-cli" "$winget_installer"
require_grep "PackageIdentifier: OpenCLICollective.codereview-cli" "$winget_locale"
require_grep "PortableCommandAlias: cr" "$winget_installer"
require_grep "cr_v0.0.0_windows_amd64.zip" "$winget_installer"
require_grep "cr_v0.0.0_windows_arm64.zip" "$winget_installer"

require_file "packaging/chocolatey/codereview-cli.nuspec"
require_file "packaging/chocolatey/tools/chocolateyInstall.ps1"
require_grep "<id>codereview-cli</id>" "packaging/chocolatey/codereview-cli.nuspec"
require_grep 'releases/download/v${version}' "packaging/chocolatey/tools/chocolateyInstall.ps1"
require_grep 'cr_v${version}_windows_${arch}.zip' "packaging/chocolatey/tools/chocolateyInstall.ps1"

require_grep 'homebrew-tap-token: ${{ secrets.TAP_GITHUB_TOKEN }}' ".github/workflows/release.yml"
require_grep 'chocolatey-api-key: ${{ secrets.CHOCOLATEY_API_KEY }}' ".github/workflows/release.yml"
require_grep 'winget-token: ${{ secrets.WINGET_GITHUB_TOKEN }}' ".github/workflows/release.yml"
require_grep 'linux-dispatch-token: ${{ secrets.LINUX_PACKAGES_DISPATCH_TOKEN }}' ".github/workflows/release.yml"

echo "package render check OK"
