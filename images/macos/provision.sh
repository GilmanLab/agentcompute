#!/usr/bin/env bash
# Provision the macOS seed guest: base assertions, an automation public key,
# the pinned Cua Driver, and its login-session LaunchAgent.
#
# Runs on the Apple Silicon host against a RUNNING Lume VM. Idempotent: every
# step is safe to repeat against an already provisioned seed.
#
# It deliberately does NOT grant TCC. Accessibility and Screen Recording need a
# human in the guest's graphical session; see README.md, then run verify.sh.
#
# Usage:
#   images/macos/provision.sh --vm <name> --key <public-key-file>
#                             --recipe <train>/desktop/image.yaml
#                             [--pins <file>]
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=images/macos/lib.sh
. "$here/lib.sh"

vm=""
key_file=""
recipe=""
pins="$here/pins.lock.yaml"

while [ $# -gt 0 ]; do
  case $1 in
    --vm) vm=${2:?--vm needs a value}; shift 2 ;;
    --key) key_file=${2:?--key needs a value}; shift 2 ;;
    --pins) pins=${2:?--pins needs a value}; shift 2 ;;
    --recipe) recipe=${2:?--recipe needs a value}; shift 2 ;;
    -h|--help) sed -n '2,14p' "$BASH_SOURCE"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ -n "$vm" ] || die "--vm is required"
[ -n "$key_file" ] || die "--key is required (the PUBLIC half only)"
[ -r "$key_file" ] || die "cannot read $key_file"
[ -n "$recipe" ] || die "--recipe is required (for example $here/tahoe/desktop/image.yaml)"
[ -r "$recipe" ] || die "cannot read $recipe"
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

launch_label=com.trycua.cua_driver_daemon

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

log "settling macOS first-login setup"
guest_run "$vm" "GUEST_USER=$LUME_GUEST_USER" "GUEST_PASSWORD=$LUME_GUEST_PASSWORD" <<'SCRIPT'
sudo_pw() { printf '%s\n' "$GUEST_PASSWORD" | sudo -S -p '' "$@"; }
plist=/Library/Preferences/com.apple.loginwindow.plist

# Lume's offline unattended setup enables autologin but does not complete Setup
# Assistant: macOS still runs its MiniBuddy flow at the account's first login,
# full-screen, before the desktop is usable. Worse for this image, a clone
# re-runs it from the Apple Account step even after the seed completed it, so a
# disposable worker would hand an agent an onboarding wizard instead of a
# desktop.
#
# loginwindow tracks that per account in AccountInfo:FirstLogins. Raising the
# count past its first-login value is what actually suppresses the flow -- the
# documented com.apple.SetupAssistant "DidSee*" keys do not, measured on
# 26.6.2. This is plain preference state, not a TCC or SIP bypass.
current=$(sudo_pw /usr/libexec/PlistBuddy -c "Print :AccountInfo:FirstLogins:$GUEST_USER" "$plist" 2>/dev/null || echo 0)
case "$current" in
  ''|*[!0-9]*) current=0 ;;
esac
if [ "$current" -lt 2 ]; then
  sudo_pw /usr/libexec/PlistBuddy -c "Set :AccountInfo:FirstLogins:$GUEST_USER 2" "$plist" >/dev/null 2>&1 ||
    sudo_pw /usr/libexec/PlistBuddy -c "Add :AccountInfo:FirstLogins:$GUEST_USER integer 2" "$plist" >/dev/null
  echo "first-login flow suppressed for $GUEST_USER (was $current)"
else
  echo "first-login flow already suppressed for $GUEST_USER ($current)"
fi

# The seed must also not drift: a guest that installs its own macOS updates
# stops matching the pinned build the recipe asserts.
sudo_pw softwareupdate --schedule off >/dev/null 2>&1 || true
SCRIPT

log "installing the Driver LaunchAgent"
guest_run "$vm" "LABEL=$launch_label" "APP_PATH=$app_path" <<'SCRIPT'
plist="$HOME/Library/LaunchAgents/$LABEL.plist"
mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
cat >"$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$APP_PATH/Contents/MacOS/cua-driver</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/cua-driver-daemon.out.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/cua-driver-daemon.err.log</string>
</dict>
</plist>
PLIST
plutil -lint "$plist" >/dev/null

# The agent execs the binary inside the app bundle, so launchd is the parent and
# the daemon is its own responsible process: `permissions status` then reports
# attribution "driver-daemon" with bundle id com.trycua.driver and
# responsible_ppid 1. Starting it with `open -g -a CuaDriver --args serve`
# instead makes `open` the responsible process, and the driver refuses to read
# its own TCC state ("no CuaDriver daemon is running under the driver's own
# identity") even though the process is the same binary. The label is the one
# `cua-driver diagnose` looks for.
launchctl bootout "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$(id -u)" "$plist"
launchctl kickstart -k "gui/$(id -u)/$LABEL"
sleep 3
# `cua-driver status | head` makes the CLI panic on a broken pipe, so read it whole.
status=$(/usr/local/bin/cua-driver status 2>&1)
printf '%s\n' "$status" | sed -n '1,3p'
echo "launch agent loaded: $LABEL"
SCRIPT

log "provisioned $vm"
cat <<NEXT

Next, and only a human can do it (see $(dirname "$recipe")/../../README.md):
  # open the guest console: 'lume attach $vm' from a GUI session, or the
  # vnc:// URL that 'lume get $vm --format json' reports
  /usr/local/bin/cua-driver permissions grant
  /usr/local/bin/cua-driver call list_apps '{}'
  /usr/local/bin/cua-driver call get_desktop_state '{}'

Then gate the seed:
  $here/verify.sh --vm $vm --recipe $recipe
NEXT
