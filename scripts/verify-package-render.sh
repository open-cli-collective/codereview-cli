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
require_grep 'args: ["catalog", "update"]' "$cask"
require_grep 'must_succeed: false' "$cask"
require_grep 'cr catalog update failed; run `cr catalog update`' "$cask"

# Exercise the rendered Ruby control flow as well as checking its source. The
# updater is warning-only during package installation, so a failed refresh
# must call opoo without raising or skipping the command.
ruby - "$cask" <<'RUBY'
path = ARGV.fetch(0)
source = File.read(path)
body = source[/  postflight do\n(.*?)\n  end/m, 1]
raise "postflight body missing" unless body

HookResult = Struct.new(:ok) do
  def success?
    ok
  end
end

class HookHarness
  attr_reader :calls, :warnings

  def initialize(fail_refresh)
    @fail_refresh = fail_refresh
    @calls = []
    @warnings = []
  end

  def staged_path
    "/tmp/staged"
  end

  def system_command(command, args:, must_succeed: true)
    @calls << {command: command, args: args, must_succeed: must_succeed}
    failed_refresh = @fail_refresh && command == "#{staged_path}/cr"
    HookResult.new(!failed_refresh)
  end

  def opoo(message)
    @warnings << message
  end

  def run(rendered_body)
    instance_eval(rendered_body, "rendered-cask", 1)
  end
end

success = HookHarness.new(false)
success.run(body)
raise "success path did not invoke both commands" unless success.calls.length == 2
raise "success path emitted a warning" unless success.warnings.empty?
raise "refresh command is not warning-only" unless success.calls.last[:must_succeed] == false

failure = HookHarness.new(true)
failure.run(body)
raise "failure path did not invoke both commands" unless failure.calls.length == 2
raise "failure path did not warn" unless failure.warnings.length == 1
raise "failure path did not keep refresh warning-only" unless failure.calls.last[:must_succeed] == false
puts "rendered package hook behavior check OK"
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

# Winget keeps version/checksum placeholders; Chocolatey keeps URL/checksum
# placeholders. The shared release workflow substitutes them before publish.
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
require_grep 'URL_AMD64_PLACEHOLDER' "packaging/chocolatey/tools/chocolateyInstall.ps1"
require_grep 'URL_ARM64_PLACEHOLDER' "packaging/chocolatey/tools/chocolateyInstall.ps1"

require_grep 'homebrew-tap-token: ${{ secrets.TAP_GITHUB_TOKEN }}' ".github/workflows/release.yml"
require_grep 'chocolatey-api-key: ${{ secrets.CHOCOLATEY_API_KEY }}' ".github/workflows/release.yml"
require_grep 'winget-token: ${{ secrets.WINGET_GITHUB_TOKEN }}' ".github/workflows/release.yml"
require_grep 'linux-dispatch-token: ${{ secrets.LINUX_PACKAGES_DISPATCH_TOKEN }}' ".github/workflows/release.yml"

echo "package render check OK"
