#!/usr/bin/env bash
# OVN connectivity probe for two already-placed disposable guests, or a full
# create/probe/destroy cycle.
#
# Usage:
#   spikes/ovn/probe.sh <remote> <project> <network> <instance1> <instance2> \
#     <forward-url> <expected-body>
#   spikes/ovn/probe.sh cycle --evidence-dir PATH [--keep-on-failure]
#
# Positional checks: network list/show; network type ovn; both instances Running
# on different members; eth0 attached to the given network with a global IPv4;
# guest eth0 MTU; cross-member ping both ways; gateway ping and DNS lookup from
# both guests; curl internet egress from both guests; workstation GET of
# forward-url matches expected-body exactly; guest HTTP GET of 100000000-byte
# /large.bin both ways with sha256 match.
#
# Cycle: one topology.py create, diagnostics, this probe, teardown. Default
# destroys owned resources even on failure. --keep-on-failure leaves them.
# Does not stop central, run keeper, or clear neighbor/ARP state.
#
# Dependencies: bash, incus, curl, python3, jq. Guests need ping, nslookup,
# curl, sha256sum. Bounds: incus 90s; ping -c 3 -W 2; curl --connect-timeout 5
# --max-time 15 (large guest GET --max-time 60). Prints commands, output,
# elapsed_seconds. Writes /tmp/ovn-large.bin in each guest; positional mode
# does not create Incus resources. Cycle writes evidence-dir/cycle.json.
set -euo pipefail

usage() { sed -n '2,25p' "$0"; }
die() { printf 'probe: %s\n' "$*" >&2; exit 1; }

RUN_OUT=""
run() {
	python3 - "$RUN_OUT" "$@" <<'PY'
import shlex, subprocess, sys, time
path, args = sys.argv[1], sys.argv[2:]
print("+ " + shlex.join(args), flush=True)
t = time.monotonic()
try:
    r = subprocess.run(args, capture_output=True, timeout=90, stdin=subprocess.DEVNULL)
except subprocess.TimeoutExpired:
    open(path, "w").write("")
    print("elapsed_seconds=%.3f exit=timeout" % (time.monotonic() - t))
    sys.exit(1)
open(path, "wb").write(r.stdout)
sys.stdout.write(r.stdout.decode(errors="replace"))
if r.stderr:
    sys.stdout.write(r.stderr.decode(errors="replace"))
print("elapsed_seconds=%.3f exit=%d" % (time.monotonic() - t, r.returncode))
sys.exit(r.returncode)
PY
}

field() {
	jq -er --arg n "$1" "$2" <<<"$list_json"
}

case "${1-}" in
-h | --help)
	usage
	exit 0
	;;
cycle)
	shift
	command -v python3 >/dev/null || die "need python3 on PATH"
	here="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
	exec python3 "$here/cycle.py" "$@"
	;;
--*) die "unknown option: $1" ;;
esac
[ "$#" -eq 7 ] || { usage >&2; die "expected remote project network instance1 instance2 forward-url expected-body"; }

remote="${1%:}"
project="$2"
network="$3"
instance1="$4"
instance2="$5"
forward_url="$6"
expected_body="$7"

[ -n "$remote" ] && [ -n "$project" ] && [ -n "$network" ] || die "remote, project, and network are required"
[ -n "$instance1" ] && [ -n "$instance2" ] && [ "$instance1" != "$instance2" ] || die "instance1 and instance2 must be distinct"
[ -n "$forward_url" ] || die "forward-url is empty"
case "$forward_url" in *://*@*) die "forward-url must not contain credentials" ;; esac
command -v incus >/dev/null && command -v curl >/dev/null && command -v python3 >/dev/null && command -v jq >/dev/null ||
	die "need bash, incus, curl, python3, and jq on PATH"

RUN_OUT="$(mktemp -t ovn-probe-XXXXXX)"
trap 'rm -f "$RUN_OUT"' EXIT
prefix="${remote}:"

run incus network list "$prefix" --project "$project" || die "network list failed"
run incus network show "${prefix}${network}" --project "$project" || die "network show ${network} failed"
[ "$(awk '/^type:/{print $2; exit}' "$RUN_OUT")" = ovn ] ||
	die "network ${network} type is not ovn"
dns="$(awk '/dns.nameservers:/{print $2; exit}' "$RUN_OUT")"
[ -n "$dns" ] || die "network ${network} has no dns.nameservers"
printf 'dns.nameservers=%s\n' "$dns"

run incus list "$prefix" --project "$project" --format json || die "instance list failed"
list_json="$(cat "$RUN_OUT")"

check_instance() {
	local name="$1" status nic
	status="$(field "$name" '.[] | select(.name==$n) | .status')" || die "instance ${name} not found"
	inst_location="$(field "$name" '.[] | select(.name==$n) | .location // empty')" || die "instance ${name} has no member location"
	nic="$(field "$name" '.[] | select(.name==$n) | .expanded_devices.eth0.network // .devices.eth0.network // empty')" || die "instance ${name} has no managed eth0 network"
	inst_ip="$(field "$name" '.[] | select(.name==$n) | [.state.network.eth0.addresses[]? | select(.family=="inet" and .scope=="global") | .address] | .[0] // empty')" || die "instance ${name} has no global IPv4 on eth0"
	[ "$status" = Running ] || die "instance ${name} status is ${status:-unknown}, want Running"
	[ -n "$inst_location" ] || die "instance ${name} has empty location"
	[ "$nic" = "$network" ] || die "instance ${name} eth0 network is ${nic:-none}, want ${network}"
	[ -n "$inst_ip" ] || die "instance ${name} has no global IPv4 on eth0"
	printf 'instance %s status=%s location=%s eth0_network=%s eth0_ipv4=%s\n' \
		"$name" "$status" "$inst_location" "$nic" "$inst_ip"
}

inst_location=""
inst_ip=""
check_instance "$instance1"
location1="$inst_location"
ip1="$inst_ip"
check_instance "$instance2"
location2="$inst_location"
ip2="$inst_ip"
[ "$location1" != "$location2" ] || die "instances share member ${location1}; want different placement"

guest() { run incus exec "${prefix}$1" --project "$project" -- "${@:2}"; }
guest "$instance1" cat /sys/class/net/eth0/mtu || die "failed to read eth0 MTU on ${instance1}"
mtu1="$(tr -d '[:space:]' <"$RUN_OUT")"
guest "$instance2" cat /sys/class/net/eth0/mtu || die "failed to read eth0 MTU on ${instance2}"
mtu2="$(tr -d '[:space:]' <"$RUN_OUT")"
printf 'guest_mtu %s=%s %s=%s\n' "$instance1" "$mtu1" "$instance2" "$mtu2"
[ -n "$mtu1" ] && [ -n "$mtu2" ] || die "empty eth0 MTU"

failures=0
failed() {
	printf 'probe: %s\n' "$*" >&2
	failures=$((failures + 1))
}

guest "$instance1" ping -c 3 -W 2 "$ip2" || failed "ping ${instance1} -> ${ip2} failed"
guest "$instance2" ping -c 3 -W 2 "$ip1" || failed "ping ${instance2} -> ${ip1} failed"

guest "$instance1" ping -c 3 -W 2 "$dns" || failed "gateway ping ${instance1} -> ${dns} failed"
guest "$instance2" ping -c 3 -W 2 "$dns" || failed "gateway ping ${instance2} -> ${dns} failed"
guest "$instance1" nslookup example.com "$dns" || failed "dns lookup example.com from ${instance1} via ${dns} failed"
guest "$instance2" nslookup example.com "$dns" || failed "dns lookup example.com from ${instance2} via ${dns} failed"

egress=(curl -4 -fsS --connect-timeout 5 --max-time 15 -o /dev/null https://example.com)
guest "$instance1" "${egress[@]}" || failed "internet egress curl from ${instance1} failed"
guest "$instance2" "${egress[@]}" || failed "internet egress curl from ${instance2} failed"

if run curl -4 -fsS --connect-timeout 5 --max-time 15 "$forward_url"; then
	python3 -c '
import sys
actual = open(sys.argv[1], "rb").read()
expected = sys.argv[2].encode()
if actual != expected:
    sys.stderr.write("probe: forward-url body mismatch: got %d bytes, expected %d\n" % (len(actual), len(expected)))
    sys.exit(1)
' "$RUN_OUT" "$expected_body" || failed "forward-url body did not match expected-body"
else
	failed "workstation curl of forward-url failed"
fi

expected_hash="a993f8c574e0fea8c1cdcbcd9408d9e2e107ee6e4d120edcfa11decd53fa0cae"
hash1=""
hash2=""
if guest "$instance1" curl -4 -fsS --connect-timeout 5 --max-time 60 -w 'bytes=%{size_download} seconds=%{time_total} bytes_per_second=%{speed_download}\n' -o /tmp/ovn-large.bin "http://${ip2}:8080/large.bin" &&
	guest "$instance1" sha256sum /tmp/ovn-large.bin; then
	hash1="$(awk '{print $1}' "$RUN_OUT")"
	[ "$hash1" = "$expected_hash" ] || failed "large GET ${instance1} <- ${ip2} sha256 ${hash1} != ${expected_hash}"
else
	failed "large GET/hash ${instance1} <- ${ip2} failed"
fi
if guest "$instance2" curl -4 -fsS --connect-timeout 5 --max-time 60 -w 'bytes=%{size_download} seconds=%{time_total} bytes_per_second=%{speed_download}\n' -o /tmp/ovn-large.bin "http://${ip1}:8080/large.bin" &&
	guest "$instance2" sha256sum /tmp/ovn-large.bin; then
	hash2="$(awk '{print $1}' "$RUN_OUT")"
	[ "$hash2" = "$expected_hash" ] || failed "large GET ${instance2} <- ${ip1} sha256 ${hash2} != ${expected_hash}"
else
	failed "large GET/hash ${instance2} <- ${ip1} failed"
fi
if [ "$hash1" = "$expected_hash" ] && [ "$hash2" = "$expected_hash" ]; then
	printf 'large_transfer bytes=100000000 sha256=%s both_directions=ok\n' "$expected_hash"
fi

[ "$failures" -eq 0 ] || die "${failures} datapath checks failed"
printf 'probe passed placement=%s:%s ipv4=%s:%s\n' "$location1" "$location2" "$ip1" "$ip2"
