#!/usr/bin/env bash
# Gate for the macOS seed and for every clone made from it.
#
# Runs on the Apple Silicon host against a RUNNING Lume VM and answers one
# question: can an agent drive this guest's desktop right now, with no operator
# present and no consent dialog? Every check is read-only except the screenshot
# it takes and removes.
#
# This is the qualification gate referenced by README.md. Run it against the
# seed after consent, and against a fresh clone to prove the TCC grants
# survived cloning.
#
# Usage:
#   images/macos/sequoia/desktop/verify.sh --vm <name> [--identity <private-key>]
#                                          [--pins <file>] [--recipe <file>]
#                                          [--no-screen-sharing]
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=images/macos/lib.sh
. "$here/../../lib.sh"

vm=""
identity=""
pins="$here/../../pins.lock.yaml"
recipe="$here/image.yaml"
screen_sharing=1

while [ $# -gt 0 ]; do
  case $1 in
    --vm) vm=${2:?--vm needs a value}; shift 2 ;;
    --identity) identity=${2:?--identity needs a value}; shift 2 ;;
    --pins) pins=${2:?--pins needs a value}; shift 2 ;;
    --recipe) recipe=${2:?--recipe needs a value}; shift 2 ;;
    --no-screen-sharing) screen_sharing=0; shift ;;
    -h|--help) sed -n '2,16p' "$BASH_SOURCE"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ -n "$vm" ] || die "--vm is required"
require_cmd lume jq awk base64
require_running "$vm"

expect_version=$(pin "$recipe" expect product_version)
expect_build=$(pin "$recipe" expect build_version)
expect_arch=$(pin "$recipe" expect architecture)
driver_version=$(pin "$pins" cua_driver version)
driver_bundle=$(pin "$pins" cua_driver bundle_id)
driver_team=$(pin "$pins" cua_driver team_identifier)
app_path=$(pin "$pins" cua_driver app_install_path)
launch_label=io.gilman.agentcompute.cua-driver

failures=0
check() {
  local name=$1
  shift
  if "$@"; then
    printf 'PASS  %s\n' "$name"
  else
    printf 'FAIL  %s\n' "$name"
    failures=$((failures + 1))
  fi
}

# json_between extracts the payload a guest script wrapped in sentinels, so
# transport noise on the `lume ssh` channel cannot corrupt a jq parse.
json_between() {
  sed -n '/^---AC-JSON---$/,/^---AC-END---$/p' | sed '1d;$d'
}

check_base() {
  guest_run "$vm" \
    "WANT_VERSION=$expect_version" "WANT_BUILD=$expect_build" "WANT_ARCH=$expect_arch" <<'SCRIPT'
[ "$(sw_vers -productVersion)" = "$WANT_VERSION" ] || { echo "productVersion mismatch" >&2; exit 1; }
[ "$(sw_vers -buildVersion)" = "$WANT_BUILD" ] || { echo "buildVersion mismatch" >&2; exit 1; }
[ "$(uname -m)" = "$WANT_ARCH" ] || { echo "architecture mismatch" >&2; exit 1; }
SCRIPT
}

check_driver_identity() {
  guest_run "$vm" \
    "APP_PATH=$app_path" "WANT_VERSION=$driver_version" \
    "WANT_BUNDLE=$driver_bundle" "WANT_TEAM=$driver_team" <<'SCRIPT'
[ -d "$APP_PATH" ] || { echo "$APP_PATH is missing" >&2; exit 1; }
codesign --verify --deep --strict "$APP_PATH"
bundle=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$APP_PATH/Contents/Info.plist")
team=$(codesign -dv "$APP_PATH" 2>&1 | awk -F= '/^TeamIdentifier=/ {print $2}')
[ "$bundle" = "$WANT_BUNDLE" ] || { echo "bundle id $bundle != $WANT_BUNDLE" >&2; exit 1; }
[ "$team" = "$WANT_TEAM" ] || { echo "team $team != $WANT_TEAM" >&2; exit 1; }
for cli in /usr/local/bin/cua-driver "$HOME/.local/bin/cua-driver"; do
  target=$(readlink "$cli" || true)
  [ "$target" = "$APP_PATH/Contents/MacOS/cua-driver" ] ||
    { echo "$cli points at '$target'" >&2; exit 1; }
done
version=$(/usr/local/bin/cua-driver --version | awk '{print $NF}')
case "$version" in *"$WANT_VERSION"*) : ;; *) echo "driver $version != $WANT_VERSION" >&2; exit 1 ;; esac
SCRIPT
}

check_launch_agent() {
  guest_run "$vm" "LABEL=$launch_label" <<'SCRIPT'
launchctl print "gui/$(id -u)/$LABEL" >/dev/null
SCRIPT
}

# The daemon, not the shell, owns the TCC grants: `source.attribution` must be
# the driver daemon. A `host` attribution means something else answered and the
# grant evidence does not apply to the app-owned identity.
check_permissions() {
  local status
  status=$(guest_run "$vm" <<'SCRIPT' | json_between
echo '---AC-JSON---'
/usr/local/bin/cua-driver permissions status --json
echo '---AC-END---'
SCRIPT
  )
  [ -n "$status" ] || { echo "no permissions payload" >&2; return 1; }
  printf '%s' "$status" | jq -e '
    .accessibility == true
    and .screen_recording == true
    and .source.attribution == "driver-daemon"
  ' >/dev/null
}

check_accessibility_tree() {
  local tree
  tree=$(guest_run "$vm" <<'SCRIPT' | json_between
echo '---AC-JSON---'
/usr/local/bin/cua-driver call get_accessibility_tree '{}'
echo '---AC-END---'
SCRIPT
  )
  [ -n "$tree" ] || { echo "empty accessibility tree" >&2; return 1; }
  printf '%s' "$tree" | jq -e 'if type == "object" then (. | length) > 0 else length > 0 end' >/dev/null
}

# Proves the exact path the server uses: one-shot `cua-driver call` with
# --screenshot-out-file, then a binary pull. With --identity it pulls over scp
# from the guest's NAT address, which is the only route the Mac host has.
check_screenshot() {
  local guest_png="/tmp/ac-verify-$$.png" report local_png address
  report=$(guest_run "$vm" "PNG=$guest_png" <<'SCRIPT' | json_between
/usr/local/bin/cua-driver call --screenshot-out-file "$PNG" get_desktop_state '{}' >/dev/null
magic=$(xxd -p -l 4 "$PNG")
bytes=$(stat -f %z "$PNG")
echo '---AC-JSON---'
printf '{"magic":"%s","bytes":%s}\n' "$magic" "$bytes"
echo '---AC-END---'
SCRIPT
  )
  printf '%s' "$report" | jq -e '.magic == "89504e47" and .bytes > 1024' >/dev/null || {
    echo "guest screenshot is not a PNG: $report" >&2
    guest_run "$vm" "PNG=$guest_png" <<<'rm -f "$PNG"' || true
    return 1
  }

  if [ -n "$identity" ]; then
    address=$(guest_address "$vm")
    [ -n "$address" ] || { echo "no NAT address for $vm" >&2; return 1; }
    local_png=$(mktemp /tmp/ac-verify.XXXXXX.png)
    scp -q -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
      -i "$identity" "$LUME_GUEST_USER@$address:$guest_png" "$local_png" || {
      echo "scp from $address failed" >&2
      return 1
    }
    file "$local_png" | grep -q 'PNG image data' || {
      echo "pulled file is not a PNG" >&2
      rm -f "$local_png"
      return 1
    }
    printf 'pulled %s (%s bytes) over scp from %s\n' \
      "$local_png" "$(stat -f %z "$local_png")" "$address"
  else
    printf 'note: --identity not given, skipping the scp pull the server uses\n'
  fi

  guest_run "$vm" "PNG=$guest_png" <<<'rm -f "$PNG"'
}

check_screen_sharing() {
  guest_run "$vm" <<'SCRIPT'
nc -z -G 2 127.0.0.1 5900
SCRIPT
}

log "verifying $vm at $(guest_address "$vm")"
check "guest base matches the pinned IPSW" check_base
check "driver app identity matches the pin" check_driver_identity
check "driver LaunchAgent is loaded" check_launch_agent
check "TCC grants are held by the driver daemon" check_permissions
check "accessibility tree is readable" check_accessibility_tree
check "desktop capture and file pull work" check_screenshot
if [ "$screen_sharing" -eq 1 ]; then
  check "screen sharing is listening" check_screen_sharing
fi
# Second pass over the consent-sensitive calls. A grant that only works once is
# a prompt that a human happened to answer, not a seeded grant.
check "grants survive a repeat call" check_permissions
check "accessibility tree survives a repeat call" check_accessibility_tree

if [ "$failures" -ne 0 ]; then
  die "$failures check(s) failed for $vm"
fi
log "$vm is qualified"
