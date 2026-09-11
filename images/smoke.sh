#!/usr/bin/env bash
# Boot smoke test for a router image that has NOT been published yet.
#
# `cmd/image-publish` publishes a verified GHCR release; the server catalog
# reconciler imports, boots, and promotes the shared `router` alias.
# This script covers the gap those tools leave open — proving a
# freshly built router.tar.xz actually boots and carries its router tooling
# BEFORE anything is pushed to a registry. Consequently it:
#
#   * never promotes (or touches) the shared `router` alias, and asserts that
#     the alias target is unchanged when it is done;
#   * imports the candidate image only when the Incus server does not already
#     have that fingerprint, and deletes ONLY what this run created;
#   * fails the run when cleanup fails, so leaked instances/images are loud.
#
# Usage:
#   images/smoke.sh --file <router.tar.xz> --remote <name> --project <name>
#                          [--suffix <token>] [--log <path>] [--timeout <seconds>]
#
# Requirements: the `incus` client on PATH (CI installs the SHA-pinned binary,
# see .github/workflows/images-publish.yml), python3 (stdlib only), sha256sum,
# and INCUS_CONF/remote configuration that can reach the server.
#
# stdout is a single JSON object describing the run; progress and guest command
# output go to stderr and to the log file.

set -euo pipefail

die() {
	printf 'smoke: %s\n' "$*" >&2
	exit 1
}

note() {
	printf 'smoke: %s\n' "$*" >&2
}

file=""
remote=""
project=""
suffix=""
log=""
timeout=180

while [ "$#" -gt 0 ]; do
	case "$1" in
	--file)
		file="${2:-}"
		shift 2
		;;
	--remote)
		remote="${2:-}"
		shift 2
		;;
	--project)
		project="${2:-}"
		shift 2
		;;
	--suffix)
		suffix="${2:-}"
		shift 2
		;;
	--log)
		log="${2:-}"
		shift 2
		;;
	--timeout)
		timeout="${2:-}"
		shift 2
		;;
	-h | --help)
		sed -n '2,30p' "$0"
		exit 0
		;;
	*)
		die "unknown argument: $1"
		;;
	esac
done

[ -n "$file" ] || die "--file is required"
[ -n "$remote" ] || die "--remote is required"
[ -n "$project" ] || die "--project is required"
[ -f "$file" ] || die "image file not found: $file"
[ "$project" != "default" ] || die "--project must not be the default project"
command -v incus >/dev/null 2>&1 || die "incus client not found on PATH"
command -v python3 >/dev/null 2>&1 || die "python3 not found on PATH"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum not found on PATH"
case "$timeout" in
'' | *[!0-9]*) die "--timeout must be a whole number of seconds" ;;
esac

if [ -z "$suffix" ]; then
	suffix="$(date -u +%Y%m%d%H%M%S)-$$"
fi
# Incus instance and alias names: lowercase alphanumerics and dashes only.
suffix="$(printf '%s' "$suffix" | tr '[:upper:]_.' '[:lower:]--' | tr -cd 'a-z0-9-')"
[ -n "$suffix" ] || die "--suffix contains no usable characters"
name="router-smoke-$suffix"
case "$name" in
[a-z0-9]*) ;;
*) die "derived name is not a valid Incus name: $name" ;;
esac
[ "${#name}" -le 63 ] || die "derived name is longer than 63 characters: $name"

if [ -z "$log" ]; then
	log="$(mktemp -t router-smoke-XXXXXX.log)"
fi
: >"$log"

# The Incus fingerprint of a unified image is the SHA-256 of the tarball itself,
# which is also the content digest `cmd/image-publish` publishes and verifies.
fingerprint="$(sha256sum "$file" | cut -d' ' -f1)"

incus_q() {
	incus --quiet --project "$project" "$@"
}

api() {
	incus query "$remote:$1?project=$project&recursion=1"
}

has_fingerprint() {
	api /1.0/images | python3 -c '
import json, sys
wanted = sys.argv[1]
images = json.load(sys.stdin) or []
sys.exit(0 if any(image.get("fingerprint") == wanted for image in images) else 1)
' "$fingerprint"
}

router_alias_target() {
	api /1.0/images/aliases | python3 -c '
import json, sys
for alias in json.load(sys.stdin) or []:
    if alias.get("name") == "router":
        print(alias.get("target", ""))
        break
else:
    print("")
'
}

image_imported=0
alias_created=0
instance_created=0
router_alias_before=""

cleanup() {
	status=$?
	trap - EXIT
	failures=0

	if [ "$instance_created" -eq 1 ]; then
		note "deleting instance $name"
		if incus_q delete -f "$remote:$name"; then
			instance_created=0
		else
			note "FAILED to delete instance $name"
			failures=1
		fi
	fi

	if [ "$alias_created" -eq 1 ]; then
		note "deleting temporary alias $name"
		if incus_q image alias delete "$remote:$name"; then
			alias_created=0
		else
			note "FAILED to delete alias $name"
			failures=1
		fi
	fi

	# Only ever delete the candidate image when THIS run imported it; a
	# fingerprint that was already on the server is shared state.
	if [ "$image_imported" -eq 1 ]; then
		note "deleting image $fingerprint imported by this run"
		if incus_q image delete "$remote:$fingerprint"; then
			image_imported=0
		else
			note "FAILED to delete image $fingerprint"
			failures=1
		fi
	fi

	if router_alias_after="$(router_alias_target)"; then
		if [ "$router_alias_after" != "$router_alias_before" ]; then
			note "FAILED invariant: router alias changed from '${router_alias_before:-<absent>}' to '${router_alias_after:-<absent>}'"
			failures=1
		fi
	else
		note "FAILED to re-read the router alias for the promotion invariant"
		failures=1
	fi

	if [ "$failures" -ne 0 ]; then
		note "cleanup failed; the project may need manual inspection"
		exit 1
	fi

	exit "$status"
}

note "candidate $file fingerprint $fingerprint"
router_alias_before="$(router_alias_target)" ||
	die "cannot reach $remote (project $project); check INCUS_CONF and network"
note "router alias currently targets ${router_alias_before:-<absent>}"

trap cleanup EXIT

if has_fingerprint; then
	note "server already has fingerprint $fingerprint; leaving that image untouched"
else
	note "importing candidate as $name"
	if incus_q image import "$file" "$remote:" --alias "$name"; then
		image_imported=1
		alias_created=1
	else
		# Another run may have imported the same fingerprint concurrently.
		has_fingerprint || die "image import failed for $fingerprint"
		note "import lost a race; the fingerprint is present and stays untouched"
	fi
	has_fingerprint || die "server does not report fingerprint $fingerprint after import"
fi

note "creating instance $name from $fingerprint"
incus_q init "$remote:$fingerprint" "$remote:$name"
instance_created=1
incus_q start "$remote:$name"

deadline=$(($(date +%s) + timeout))
until incus_q exec "$remote:$name" -- /bin/true >/dev/null 2>&1; do
	[ "$(date +%s)" -lt "$deadline" ] ||
		die "instance $name was not reachable within ${timeout}s"
	sleep 2
done
note "instance $name is up"

check() {
	printf '=== %s\n' "$*" >>"$log"
	if ! incus_q exec "$remote:$name" -- "$@" 2>&1 | tee -a "$log" >&2; then
		printf '=== FAILED: %s\n' "$*" >>"$log"
		die "router tool check failed: $* (log: $log)"
	fi
}

check nft --version
check vtysh --help
check tc -V
check dnsmasq --version
check wg --version
check tcpdump --version
note "all six router tool checks passed"

printf '{"fingerprint":"%s","file":"%s","remote":"%s","project":"%s","instance":"%s","image_imported":%s,"checks":6,"log":"%s"}\n' \
	"$fingerprint" "$file" "$remote" "$project" "$name" \
	"$([ "$image_imported" -eq 1 ] && printf true || printf false)" "$log"
