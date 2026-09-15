#!/usr/bin/env bash
# Provision the macOS seed guest: base assertions, an automation public key,
# the pinned Cua Driver, its login-session LaunchAgent, and Screen Sharing.
#
# Runs on the Apple Silicon host against a RUNNING Lume VM. Idempotent: every
# step is safe to repeat against an already provisioned seed.
#
# It deliberately does NOT grant TCC. Accessibility and Screen Recording need a
# human in the guest's graphical session; see README.md, then run verify.sh.
#
# Usage:
#   images/macos/sequoia/desktop/provision.sh --vm <name> --key <public-key-file>
#                                             [--pins <file>] [--recipe <file>]
#                                             [--no-screen-sharing]
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=images/macos/lib.sh
. "$here/../../lib.sh"

vm=""
key_file=""
pins="$here/../../pins.lock.yaml"
recipe="$here/image.yaml"
screen_sharing=1

while [ $# -gt 0 ]; do
  case $1 in
    --vm) vm=${2:?--vm needs a value}; shift 2 ;;
    --key) key_file=${2:?--key needs a value}; shift 2 ;;
    --pins) pins=${2:?--pins needs a value}; shift 2 ;;
    --recipe) recipe=${2:?--recipe needs a value}; shift 2 ;;
    --no-screen-sharing) screen_sharing=0; shift ;;
    -h|--help) sed -n '2,13p' "$BASH_SOURCE"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ -n "$vm" ] || die "--vm is required"
[ -n "$key_file" ] || die "--key is required (the PUBLIC half only)"
[ -r "$key_file" ] || die "cannot read $key_file"
grep -qE '^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp[0-9]+) ' "$key_file" ||
  die "$key_file does not look like an OpenSSH public key"
grep -q 'PRIVATE KEY' "$key_file" && die "$key_file is a private key; pass the .pub file"

require_cmd lume jq awk base64
require_running "$vm"

expect_version=$(pin "$recipe" expect product_version)
expect_build=$(pin "$recipe" expect build_version)
expect_arch=$(pin "$recipe" expect architecture)

driver_version=$(pin "$pins" cua_driver version)
driver_url=$(pin "$pins" cua_driver url)
driver_sha=$(pin "$pins" cua_driver sha256)
driver_archive=$(pin "$pins" cua_driver archive)
driver_root=$(pin "$pins" cua_driver archive_root)
driver_bundle=$(pin "$pins" cua_driver bundle_id)
driver_team=$(pin "$pins" cua_driver team_identifier)
app_path=$(pin "$pins" cua_driver app_install_path)

launch_label=io.gilman.agentcompute.cua-driver

log "asserting the guest base ($expect_version / $expect_build / $expect_arch)"
guest_run "$vm" \
  "WANT_VERSION=$expect_version" "WANT_BUILD=$expect_build" "WANT_ARCH=$expect_arch" <<'SCRIPT'
got_version=$(sw_vers -productVersion)
got_build=$(sw_vers -buildVersion)
got_arch=$(uname -m)
[ "$got_version" = "$WANT_VERSION" ] || { echo "guest is macOS $got_version, want $WANT_VERSION" >&2; exit 1; }
[ "$got_build" = "$WANT_BUILD" ] || { echo "guest is build $got_build, want $WANT_BUILD" >&2; exit 1; }
[ "$got_arch" = "$WANT_ARCH" ] || { echo "guest is $got_arch, want $WANT_ARCH" >&2; exit 1; }
echo "base ok: macOS $got_version ($got_build) on $got_arch"
SCRIPT

log "installing the automation public key"
guest_run "$vm" "PUBKEY=$(cat "$key_file")" <<'SCRIPT'
mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
touch "$HOME/.ssh/authorized_keys"
chmod 600 "$HOME/.ssh/authorized_keys"
if grep -qxF "$PUBKEY" "$HOME/.ssh/authorized_keys"; then
  echo "key already present"
else
  printf '%s\n' "$PUBKEY" >> "$HOME/.ssh/authorized_keys"
  echo "key installed"
fi
SCRIPT

log "installing Cua Driver $driver_version"
guest_run "$vm" \
  "DRIVER_URL=$driver_url" "DRIVER_SHA=$driver_sha" "DRIVER_ARCHIVE=$driver_archive" \
  "DRIVER_ROOT=$driver_root" "DRIVER_VERSION=$driver_version" "DRIVER_BUNDLE=$driver_bundle" \
  "DRIVER_TEAM=$driver_team" "APP_PATH=$app_path" "GUEST_PASSWORD=$LUME_GUEST_PASSWORD" <<'SCRIPT'
sudo_pw() { printf '%s\n' "$GUEST_PASSWORD" | sudo -S -p '' "$@"; }

work=$(mktemp -d /tmp/ac-driver.XXXXXX)
trap 'rm -rf "$work"' EXIT

curl -fsSL --retry 3 -o "$work/$DRIVER_ARCHIVE" "$DRIVER_URL"
got_sha=$(shasum -a 256 "$work/$DRIVER_ARCHIVE" | awk '{print $1}')
[ "$got_sha" = "$DRIVER_SHA" ] || { echo "driver digest $got_sha != pinned $DRIVER_SHA" >&2; exit 1; }

tar xzf "$work/$DRIVER_ARCHIVE" -C "$work"
src="$work/$DRIVER_ROOT/CuaDriver.app"
[ -d "$src" ] || { echo "archive has no CuaDriver.app at $src" >&2; exit 1; }

# TCC follows the code-signing identity, which is what makes an operator's
# consent survive `lume clone`. Refuse anything but the pinned release identity.
codesign --verify --deep --strict "$src"
got_bundle=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$src/Contents/Info.plist")
got_team=$(codesign -dv "$src" 2>&1 | awk -F= '/^TeamIdentifier=/ {print $2}')
[ "$got_bundle" = "$DRIVER_BUNDLE" ] || { echo "bundle id $got_bundle != $DRIVER_BUNDLE" >&2; exit 1; }
[ "$got_team" = "$DRIVER_TEAM" ] || { echo "team $got_team != $DRIVER_TEAM" >&2; exit 1; }

sudo_pw rm -rf "$APP_PATH"
sudo_pw ditto "$src" "$APP_PATH"
sudo_pw codesign --verify --deep --strict "$APP_PATH"

# Register synchronously so `open -a CuaDriver` resolves this bundle.
lsregister=/System/Library/Frameworks/CoreServices.framework/Versions/A/Frameworks/LaunchServices.framework/Versions/A/Support/lsregister
sudo_pw "$lsregister" -f "$APP_PATH"

# Upstream's release installer symlinks ~/.local/bin/cua-driver. The extra
# /usr/local/bin symlink makes the guest CLI path identical to the Linux
# desktop image's, so the server needs no macOS-specific binary path.
mkdir -p "$HOME/.local/bin"
ln -sf "$APP_PATH/Contents/MacOS/cua-driver" "$HOME/.local/bin/cua-driver"
sudo_pw mkdir -p /usr/local/bin
sudo_pw ln -sf "$APP_PATH/Contents/MacOS/cua-driver" /usr/local/bin/cua-driver

got_driver=$(/usr/local/bin/cua-driver --version | awk '{print $NF}')
case "$got_driver" in
  *"$DRIVER_VERSION"*) : ;;
  *) echo "installed driver reports $got_driver, want $DRIVER_VERSION" >&2; exit 1 ;;
esac
echo "driver ok: $got_driver ($got_bundle, team $got_team)"
SCRIPT

log "installing the login-session LaunchAgent"
guest_run "$vm" "LABEL=$launch_label" "APP_PATH=$app_path" <<'SCRIPT'
plist="$HOME/Library/LaunchAgents/$LABEL.plist"
mkdir -p "$HOME/Library/LaunchAgents"
cat >"$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/bin/open</string>
    <string>-g</string>
    <string>-a</string>
    <string>$APP_PATH</string>
    <string>--args</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/cua-driver-serve.out.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/cua-driver-serve.err.log</string>
</dict>
</plist>
PLIST
plutil -lint "$plist" >/dev/null

# The daemon is started through LaunchServices rather than by executing the
# bundle binary directly, so the responsible process is CuaDriver.app and the
# app-owned TCC grants apply. KeepAlive is deliberately absent: `open` exits
# immediately after handing off, so a KeepAlive agent would respawn forever.
mkdir -p "$HOME/Library/Logs"
launchctl bootout "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$(id -u)" "$plist"
launchctl kickstart "gui/$(id -u)/$LABEL"
echo "launch agent loaded: $LABEL"
SCRIPT

if [ "$screen_sharing" -eq 1 ]; then
  log "enabling Screen Sharing (host-reachable VNC fallback)"
  guest_run "$vm" "GUEST_PASSWORD=$LUME_GUEST_PASSWORD" <<'SCRIPT'
sudo_pw() { printf '%s\n' "$GUEST_PASSWORD" | sudo -S -p '' "$@"; }
sudo_pw launchctl enable system/com.apple.screensharing
sudo_pw launchctl kickstart -k system/com.apple.screensharing
echo "screen sharing enabled on 5900 (guest NAT address only)"
SCRIPT
fi

log "provisioned $vm"
cat <<NEXT

Next, and only a human can do it (see README.md):
  lume attach $vm
  # in the guest's graphical session:
  /usr/local/bin/cua-driver permissions grant
  /usr/local/bin/cua-driver call list_apps '{}'
  /usr/local/bin/cua-driver call get_desktop_state '{}'

Then gate the seed:
  images/macos/sequoia/desktop/verify.sh --vm $vm
NEXT
