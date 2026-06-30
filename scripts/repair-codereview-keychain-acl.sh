#!/usr/bin/env bash
set -euo pipefail

DEFAULT_SERVICE="codereview"
EXPECTED_IDENTIFIER="org.open-cli-collective.cr"
# Source of truth: Open CLI Collective macOS release-signing MACOS_CERT_LEAF_SHA.
# Do not derive this from the candidate cr binary being validated.
EXPECTED_LEAF_SHA="42e1afd02aae8666c09c15f171e1639550f301c2"

usage() {
  cat <<'USAGE'
Usage: scripts/repair-codereview-keychain-acl.sh [--apply] [--service codereview] [--keychain PATH] [--cr PATH]
       scripts/repair-codereview-keychain-acl.sh --self-test

Find codereview Keychain generic-password items that do not yet trust the
current stable-signed cr binary. In apply mode, update cdhash-only items to trust
current cr. Items that already trust current cr are considered repaired even if
macOS still shows historical cdhash grants in ACL or partition metadata.

Default mode is dry-run. Pass --apply to modify matching Keychain items.

Run this as your normal macOS user, not with sudo. Login Keychain ACL changes are
authorized through the user's security session and may show macOS prompts.
USAGE
}

lower() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

expected_dr() {
  printf 'identifier "%s" and certificate leaf = H"%s"' "$EXPECTED_IDENTIFIER" "$EXPECTED_LEAF_SHA"
}

classify_keychain_dump() {
  perl -Mstrict -Mwarnings -e '
    my ($service, $dr) = @ARGV;
    my $block = "";

    sub flush_block {
      my ($block, $service, $dr) = @_;
      return if $block !~ /\Q"svce"<blob>="$service"\E/;
      return if $block !~ /^class: "genp"$/m;
      my ($account) = $block =~ /"acct"<blob>="([^"]*)"/;
      return if !defined $account || $account eq "";
      my @reqs = $block =~ /requirement: ([^\n]*)/g;
      my $has_current = grep { lc($_) eq lc($dr) } @reqs;
      my $has_cdhash = grep { /cdhash H"/i } @reqs;
      my $state = $has_current ? ($has_cdhash ? "stable+stale-cdhash" : "stable") :
                  ($has_cdhash ? "cdhash-only" : "missing-current-cr");
      print "$account\t$state\n";
    }

    while (my $line = <STDIN>) {
      if ($line =~ /^keychain:/) {
        flush_block($block, $service, $dr);
        $block = "";
        next;
      }
      $block .= $line;
    }
    flush_block($block, $service, $dr);
  ' "$1" "$2"
}

run_self_test() {
  command -v perl >/dev/null || { echo "perl not found" >&2; exit 2; }

  local dr fixture actual expected
  dr="$(expected_dr)"
  fixture=$(cat <<EOF
keychain: "/tmp/login.keychain-db"
class: "genp"
    "svce"<blob>="codereview"
    "type"<uint32>=<NULL>
    "acct"<blob>="stable/git_token"
        requirement: $dr
keychain: "/tmp/login.keychain-db"
class: "genp"
    "svce"<blob>="codereview"
    "type"<uint32>=<NULL>
    "acct"<blob>="mixed/git_token"
        requirement: $dr
        requirement: cdhash H"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
keychain: "/tmp/login.keychain-db"
class: "genp"
    "svce"<blob>="codereview"
    "type"<uint32>=<NULL>
    "acct"<blob>="old/git_token"
        requirement: cdhash H"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
keychain: "/tmp/login.keychain-db"
class: "genp"
    "svce"<blob>="codereview"
    "type"<uint32>=<NULL>
    "acct"<blob>="missing/git_token"
        requirement: identifier "other.tool" and certificate leaf = H"cccccccccccccccccccccccccccccccccccccccc"
keychain: "/tmp/login.keychain-db"
class: "genp"
    "svce"<blob>="other-service"
    "type"<uint32>=<NULL>
    "acct"<blob>="ignored/git_token"
        requirement: cdhash H"dddddddddddddddddddddddddddddddddddddddd"
keychain: "/tmp/login.keychain-db"
class: "genp"
    "svce"<blob>="codereview"
    "type"<uint32>=<NULL>
        requirement: cdhash H"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
keychain: "/tmp/login.keychain-db"
class: 0x0000000F
    "svce"<blob>="codereview"
    "acct"<blob>="not-generic/git_token"
        requirement: cdhash H"ffffffffffffffffffffffffffffffffffffffff"
EOF
)
  expected=$(cat <<'EOF'
missing/git_token	missing-current-cr
mixed/git_token	stable+stale-cdhash
old/git_token	cdhash-only
stable/git_token	stable
EOF
)
  actual="$(printf '%s\n' "$fixture" | classify_keychain_dump "$DEFAULT_SERVICE" "$dr" | sort)"
  if [ "$actual" != "$expected" ]; then
    echo "self-test failed: classifier output mismatch" >&2
    printf 'expected:\n%s\n' "$expected" >&2
    printf 'actual:\n%s\n' "$actual" >&2
    exit 1
  fi

  actual="$(printf '%s\n' 'malformed output without keychain blocks' | classify_keychain_dump "$DEFAULT_SERVICE" "$dr" | sort)"
  if [ -n "$actual" ]; then
    echo "self-test failed: malformed output should produce no matches" >&2
    printf 'actual:\n%s\n' "$actual" >&2
    exit 1
  fi

  echo "self-test OK"
}

build_helper() {
  command -v cc >/dev/null || { echo "cc not found; install Xcode command line tools" >&2; exit 2; }
  ensure_work_tmp
  helper="$work_tmp/kc_acl_set"

  cat >"$work_tmp/kc_acl_set.c" <<'C'
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <stdio.h>
#include <string.h>

static void print_status(const char *what, OSStatus st) {
  fprintf(stderr, "%s: %d\n", what, (int)st);
}

static int authorizations_contain(CFArrayRef auths, CFStringRef target) {
  if (auths == NULL) {
    return 0;
  }
  CFRange range = CFRangeMake(0, CFArrayGetCount(auths));
  return CFArrayContainsValue(auths, range, target);
}

int main(int argc, char **argv) {
  if (argc != 5) {
    fprintf(stderr, "usage: kc_acl_set <keychain> <service> <account> <app-path>\n");
    return 2;
  }

  const char *kc_path = argv[1];
  const char *service = argv[2];
  const char *account = argv[3];
  const char *app_path = argv[4];

  SecKeychainRef kc = NULL;
  OSStatus st = SecKeychainOpen(kc_path, &kc);
  if (st != errSecSuccess) {
    print_status("SecKeychainOpen", st);
    return 3;
  }

  SecKeychainItemRef item = NULL;
  st = SecKeychainFindGenericPassword(
      kc, (UInt32)strlen(service), service, (UInt32)strlen(account), account,
      NULL, NULL, &item);
  if (st != errSecSuccess) {
    print_status("SecKeychainFindGenericPassword", st);
    CFRelease(kc);
    return 4;
  }

  SecTrustedApplicationRef app = NULL;
  st = SecTrustedApplicationCreateFromPath(app_path, &app);
  if (st != errSecSuccess) {
    print_status("SecTrustedApplicationCreateFromPath", st);
    CFRelease(item);
    CFRelease(kc);
    return 5;
  }

  CFStringRef fallback_label =
      CFStringCreateWithCString(kCFAllocatorDefault, service, kCFStringEncodingUTF8);

  SecAccessRef access = NULL;
  st = SecKeychainItemCopyAccess(item, &access);
  if (st != errSecSuccess) {
    print_status("SecKeychainItemCopyAccess", st);
    CFRelease(fallback_label);
    CFRelease(app);
    CFRelease(item);
    CFRelease(kc);
    return 6;
  }

  CFArrayRef acl_list = NULL;
  st = SecAccessCopyACLList(access, &acl_list);
  if (st != errSecSuccess) {
    print_status("SecAccessCopyACLList", st);
    CFRelease(access);
    CFRelease(fallback_label);
    CFRelease(app);
    CFRelease(item);
    CFRelease(kc);
    return 7;
  }

  int changed = 0;
  CFIndex acl_count = CFArrayGetCount(acl_list);
  for (CFIndex i = 0; i < acl_count; i++) {
    SecACLRef acl = (SecACLRef)CFArrayGetValueAtIndex(acl_list, i);
    CFArrayRef auths = SecACLCopyAuthorizations(acl);
    if (!authorizations_contain(auths, kSecACLAuthorizationDecrypt)) {
      if (auths != NULL) {
        CFRelease(auths);
      }
      continue;
    }

    CFArrayRef existing_apps = NULL;
    CFStringRef description = NULL;
    SecKeychainPromptSelector prompt_selector = 0;
    st = SecACLCopyContents(acl, &existing_apps, &description, &prompt_selector);
    if (st != errSecSuccess) {
      print_status("SecACLCopyContents", st);
      if (auths != NULL) {
        CFRelease(auths);
      }
      CFRelease(acl_list);
      CFRelease(access);
      CFRelease(fallback_label);
      CFRelease(app);
      CFRelease(item);
      CFRelease(kc);
      return 8;
    }

    if (existing_apps == NULL) {
      if (description != NULL) {
        CFRelease(description);
      }
      if (auths != NULL) {
        CFRelease(auths);
      }
      continue;
    }

    CFMutableArrayRef new_apps =
        CFArrayCreateMutableCopy(kCFAllocatorDefault, 0, existing_apps);
    CFArrayAppendValue(new_apps, app);
    st = SecACLSetContents(acl, new_apps,
                           description == NULL ? fallback_label : description,
                           prompt_selector);
    if (st != errSecSuccess) {
      print_status("SecACLSetContents", st);
      CFRelease(new_apps);
      if (description != NULL) {
        CFRelease(description);
      }
      if (existing_apps != NULL) {
        CFRelease(existing_apps);
      }
      if (auths != NULL) {
        CFRelease(auths);
      }
      CFRelease(acl_list);
      CFRelease(access);
      CFRelease(fallback_label);
      CFRelease(app);
      CFRelease(item);
      CFRelease(kc);
      return 9;
    }
    changed = 1;

    CFRelease(new_apps);
    if (description != NULL) {
      CFRelease(description);
    }
    if (existing_apps != NULL) {
      CFRelease(existing_apps);
    }
    if (auths != NULL) {
      CFRelease(auths);
    }
  }

  if (!changed) {
    fprintf(stderr, "no decrypt-capable ACL found for item\n");
    CFRelease(acl_list);
    CFRelease(access);
    CFRelease(fallback_label);
    CFRelease(app);
    CFRelease(item);
    CFRelease(kc);
    return 10;
  }

  st = SecKeychainItemSetAccess(item, access);
  if (st != errSecSuccess) {
    print_status("SecKeychainItemSetAccess", st);
    CFRelease(acl_list);
    CFRelease(access);
    CFRelease(fallback_label);
    CFRelease(app);
    CFRelease(item);
    CFRelease(kc);
    return 11;
  }

  CFRelease(acl_list);
  CFRelease(access);
  CFRelease(fallback_label);
  CFRelease(app);
  CFRelease(item);
  CFRelease(kc);
  return 0;
}
C

  echo "Building temporary ACL helper..." >&2
  cc "$work_tmp/kc_acl_set.c" -Wno-deprecated-declarations \
    -framework Security -framework CoreFoundation -o "$helper" >/dev/null
}

cleanup() {
  if [ -n "$work_tmp" ]; then
    rm -rf "$work_tmp"
  fi
  if [ -n "$items_file" ]; then
    rm -f "$items_file"
  fi
}

ensure_work_tmp() {
  if [ -z "$work_tmp" ]; then
    work_tmp="$(mktemp -d)"
  fi
}

apply=0
self_test=0
service="$DEFAULT_SERVICE"
keychain="$HOME/Library/Keychains/login.keychain-db"
cr_bin=""
helper=""
work_tmp=""
items_file=""
trap cleanup EXIT

while [ "$#" -gt 0 ]; do
  case "$1" in
    --apply)
      apply=1
      shift
      ;;
    --self-test)
      self_test=1
      shift
      ;;
    --service)
      service="${2:?--service requires a value}"
      shift 2
      ;;
    --keychain)
      keychain="${2:?--keychain requires a value}"
      shift 2
      ;;
    --cr)
      cr_bin="${2:?--cr requires a value}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [ "$self_test" -eq 1 ]; then
  run_self_test
  exit 0
fi

if [ "$(uname -s)" != "Darwin" ]; then
  echo "This helper only supports macOS Keychain on Darwin." >&2
  exit 2
fi

if [ "${EUID:-$(id -u)}" -eq 0 ]; then
  echo "Do not run this with sudo; run it as the user who owns the login Keychain." >&2
  exit 2
fi

command -v codesign >/dev/null || { echo "codesign not found" >&2; exit 2; }
command -v security >/dev/null || { echo "security not found" >&2; exit 2; }
command -v perl >/dev/null || { echo "perl not found" >&2; exit 2; }

if [ -z "$cr_bin" ]; then
  cr_bin="$(command -v cr || true)"
fi
[ -n "$cr_bin" ] || { echo "cr not found on PATH; pass --cr /path/to/cr" >&2; exit 2; }
[ -x "$cr_bin" ] || { echo "cr is not executable: $cr_bin" >&2; exit 2; }
[ -f "$keychain" ] || { echo "keychain not found: $keychain" >&2; exit 2; }

dr="$(codesign -d -r- "$cr_bin" 2>&1 | sed -n 's/^designated => //p' | head -n 1)"
expected="$(expected_dr)"
[ -n "$dr" ] || { echo "could not read designated requirement from $cr_bin" >&2; exit 1; }
if [ "$(lower "$dr")" != "$(lower "$expected")" ]; then
  echo "refusing to repair with unexpected cr designated requirement" >&2
  echo "expected: $expected" >&2
  echo "actual:   $dr" >&2
  exit 1
fi

list_items() {
  ensure_work_tmp
  local dump_file err_file
  dump_file="$work_tmp/keychain.dump"
  err_file="$work_tmp/keychain.err"
  if ! security dump-keychain -a "$keychain" >"$dump_file" 2>"$err_file"; then
    echo "failed to scan Keychain ACL metadata: $keychain" >&2
    if [ -s "$err_file" ]; then
      sed 's/^/  /' "$err_file" >&2
    fi
    return 1
  fi
  classify_keychain_dump "$service" "$expected" <"$dump_file"
}

all_items=()
echo "Scanning Keychain ACL metadata for service '$service' in $keychain ..." >&2
echo "This can take 10-30 seconds on a large login Keychain." >&2
items_file="$(mktemp)"
list_items | sort -u >"$items_file"
while IFS= read -r item; do
  all_items+=("$item")
done <"$items_file"
echo "Scan complete." >&2

if [ "${#all_items[@]}" -eq 0 ]; then
  echo "No generic-password items found for service '$service' in $keychain."
  exit 0
fi

echo "Current cr: $cr_bin"
echo "Current DR: $dr"
echo
echo "Items:"

targets=()
manual=()
for item in "${all_items[@]}"; do
  account="${item%%$'\t'*}"
  state="${item#*$'\t'}"
  printf '  %-24s %s\n' "$state" "$account"
  case "$state" in
    stable|stable+stale-cdhash)
      ;;
    cdhash-only)
      targets+=("$account")
      ;;
    *)
      manual+=("$account")
      ;;
  esac
done

if [ "${#manual[@]}" -gt 0 ]; then
  echo
  echo "Manual inspection required for ${#manual[@]} non-cdhash item(s) missing current cr trust:"
  for account in "${manual[@]}"; do
    printf '  %s\n' "$account"
  done
fi

if [ "${#targets[@]}" -eq 0 ]; then
  echo
  echo "No cdhash-only items to repair. stable+stale-cdhash means current cr is trusted;"
  echo "macOS is only still reporting older cdhash grants/partition metadata."
  exit 0
fi

echo
if [ "$apply" -ne 1 ]; then
  echo "Dry-run only. Re-run with --apply to repair ${#targets[@]} item(s)."
  exit 0
fi

echo "Repairing ${#targets[@]} item(s). macOS may prompt for Keychain authorization."
build_helper
failures=0
for account in "${targets[@]}"; do
  printf '  repairing %s ... ' "$account"
  if "$helper" "$keychain" "$service" "$account" "$cr_bin"; then
    echo "ok"
  else
    echo "failed"
    failures=$((failures + 1))
  fi
done

echo
if [ "$failures" -ne 0 ]; then
  echo "$failures item(s) failed to repair." >&2
  exit 1
fi

echo "Repair complete. Re-run without --apply to verify all items report stable or stable+stale-cdhash."
