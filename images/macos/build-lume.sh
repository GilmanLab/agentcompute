#!/usr/bin/env bash
# Build and install the account-local Lume that can start a VM with no VNC
# listener, from the exact upstream commit pinned in pins/lume.yaml.
#
# Runs on the Apple Silicon host, AS the service account that owns the VMs
# (agentcompute), never as root and never through the global install. It
# touches only paths under that account's home: the global
# /usr/local/bin/lume (released 0.5.3) is left exactly as it is, and no
# service, LaunchDaemon, LaunchAgent, or VM is started here.
#
# Idempotent: re-running refetches the same commit, rebuilds in the same
# SwiftPM cache, and replaces the installed bundle in place.
#
# Usage, as the service account:
#   sudo -u agentcompute -H <staged>/images/macos/build-lume.sh
#   sudo -u agentcompute -H <staged>/images/macos/build-lume.sh --pins <file>
#
# The service account cannot read a developer's home directory, so a checkout
# under ~/code is not reachable from it. Stage the three files it needs in a
# world-readable directory, keeping the layout so the default --pins path
# resolves:
#
#   mkdir -p /tmp/lume-build/images/macos /tmp/lume-build/pins
#   cp images/macos/build-lume.sh images/macos/lib.sh /tmp/lume-build/images/macos/
#   cp pins/lume.yaml /tmp/lume-build/pins/
#   chmod -R a+rX /tmp/lume-build
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=images/macos/lib.sh
. "$here/lib.sh"

pins="$here/../../pins/lume.yaml"

while [ $# -gt 0 ]; do
  case "$1" in
    --pins)
      pins=${2:?--pins needs a file}
      shift 2
      ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$(id -u)" -ne 0 ] || die "run as the service account that owns the VMs, not as root"
require_cmd git swift codesign shasum ditto xattr awk sed

commit=$(pin "$pins" lume source_commit)
source_url=$(pin "$pins" lume source_url)
subpath=$(pin "$pins" lume source_subpath)
entitlements=$(pin "$pins" lume entitlements_file)
want_sha=$(pin "$pins" lume installed_sha256)
want_wrapper=$(pin "$pins" lume wrapper_sha256)
src=$(pin "$pins" lume source_dir)
app_dir=$(pin "$pins" lume app_install_dir)
install_bin=$(pin "$pins" lume install_bin)

case "$commit" in
  [0-9a-f]*) [ ${#commit} -eq 40 ] || die "$pins: lume.source_commit must be a full 40-character SHA" ;;
  *) die "$pins: lume.source_commit must be a full 40-character SHA" ;;
esac
for path in "$src" "$app_dir" "$install_bin"; do
  case "$path" in
    "$HOME"/*) ;;
    *) die "$path is outside $HOME; this build never writes outside the service account" ;;
  esac
done

log "fetching $commit from $source_url into $src"
mkdir -p "$src"
cd "$src"
[ -d .git ] || git init -q
git remote get-url origin >/dev/null 2>&1 || git remote add origin "$source_url"
# A blobless, single-commit, sparse fetch: the monorepo is large and only
# libs/lume is built here.
git fetch --depth 1 --filter=blob:none origin "$commit"
git sparse-checkout set --cone "$subpath"
git checkout -q --detach "$commit"
[ "$(git rev-parse HEAD)" = "$commit" ] || die "checkout is not $commit"

lume_dir="$src/$subpath"
cd "$lume_dir"
[ -r Package.resolved ] || die "$lume_dir/Package.resolved is missing; refusing to resolve dependencies freely"

log "building lume (release) with the dependency versions in Package.resolved"
swift build -c release --product lume --only-use-versions-from-resolved-file

# Assemble the .app the way upstream's own release build does: the resource
# bundle must sit in Contents/Resources, or codesign rejects the layout.
build_path="$lume_dir/.build/release"
app="$build_path/lume.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp -f "$build_path/lume" "$app/Contents/MacOS/lume"
[ -d "$build_path/lume_lume.bundle" ] || die "resource bundle lume_lume.bundle was not built"
cp -Rf "$build_path/lume_lume.bundle" "$app/Contents/Resources/"
cp -f "$lume_dir/resources/AppIcon.icns" "$app/Contents/Resources/AppIcon.icns"
sed "s/__VERSION__/$(cat "$lume_dir/VERSION")/g" "$lume_dir/resources/Info.plist" \
  >"$app/Contents/Info.plist"

# Ad-hoc signature, local entitlements. `com.apple.vm.networking` is
# deliberately absent: it is an Apple-granted entitlement, and macOS kills an
# ad-hoc binary that carries it. NAT networking, which this deployment uses,
# needs only com.apple.security.virtualization. No provisioning profile is
# embedded for the same reason.
#
# The bundle is signed ONCE, with the entitlements. Signing the bundle also
# signs its main executable, so entitling the inner Mach-O first (as
# scripts/build/build-release.sh does) is pointless: the bundle signature
# overwrites it and the installed binary ends up with no entitlements at all.
# Upstream's notarized release path signs the bundle with entitlements, and
# that is the order copied here.
log "signing with $entitlements"
xattr -cr "$app" 2>/dev/null || true
codesign --force --entitlements "$lume_dir/$entitlements" --sign - "$app"
codesign --verify --strict "$app"
codesign -d --entitlements - --xml "$app" 2>/dev/null |
  grep -q com.apple.security.virtualization ||
  die "signed bundle carries no virtualization entitlement"

# The pin names the artifact this host is supposed to run, so the hash is
# checked BEFORE anything is installed: a build that disagrees with the pin
# never reaches the service account's install paths. Re-pinning is a decision
# for a human, not a warning to scroll past.
got_sha=$(shasum -a 256 "$app/Contents/MacOS/lume" | awk '{print $1}')
[ "$got_sha" = "$want_sha" ] || die "built binary is $got_sha, but $pins pins $want_sha
Nothing was installed. If this build is the one you intend to deploy, re-read
the commit, then set lume.installed_sha256 to $got_sha in $pins and say why in
the commit message."

log "installing to $app_dir and $install_bin"
mkdir -p "$app_dir" "$(dirname "$install_bin")"
rm -rf "$app_dir/lume.app"
# ditto, not cp: it preserves the signature and its extended attributes.
ditto "$app" "$app_dir/lume.app"
# Replace the inode so taskgated cannot reuse a stale signature cache.
rm -f "$install_bin"
cat >"$install_bin" <<WRAPPER
#!/bin/sh
exec "$app_dir/lume.app/Contents/MacOS/lume" "\$@"
WRAPPER
chmod 755 "$install_bin"

installed="$app_dir/lume.app/Contents/MacOS/lume"
[ "$(shasum -a 256 "$installed" | awk '{print $1}')" = "$got_sha" ] ||
  die "$installed does not match the binary that was just signed"
wrapper_sha=$(shasum -a 256 "$install_bin" | awk '{print $1}')
[ "$wrapper_sha" = "$want_wrapper" ] ||
  die "$install_bin is $wrapper_sha, which is not the launcher $pins pins"

log "installed $installed"
printf 'commit:          %s\n' "$commit"
printf 'binary:          %s\n' "$installed"
printf 'binary sha256:   %s\n' "$got_sha"
printf 'launcher:        %s\n' "$install_bin"
printf 'launcher sha256: %s\n' "$wrapper_sha"
printf 'vnc policy:     %s\n' "$("$install_bin" run --help 2>/dev/null | awk '/--vnc </{print; exit}')"
