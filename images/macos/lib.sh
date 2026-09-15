#!/usr/bin/env bash
# Shared helpers for the macOS seed scripts. Source it; do not execute it.
#
# Everything here runs on the Apple Silicon host that owns the Lume VMs. Guest
# commands travel through `lume ssh`, which knows the guest's NAT address and
# the documented lume/lume account, so no guest IP or key is needed to bootstrap.

# shellcheck shell=bash

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

log() {
  printf '== %s\n' "$*"
}

require_cmd() {
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || die "$cmd is required on the host"
  done
}

# pin <file> <section> <key>
#
# Reads one scalar from the flat two-level subset of YAML these recipes use:
# a top-level `section:` line followed by indented `key: value` lines. Quotes
# and inline comments are stripped. A missing section or key is a hard error,
# so a renamed pin fails the script instead of silently defaulting.
pin() {
  local file=$1 section=$2 key=$3 value
  [ -r "$file" ] || die "cannot read $file"
  value=$(awk -v section="$section" -v key="$key" '
    index($0, section ":") == 1 { inside = 1; next }
    /^[^[:space:]#]/ { inside = 0 }
    inside {
      line = $0
      sub(/^[[:space:]]+/, "", line)
      if (index(line, key ":") == 1) {
        sub(/^[^:]+:[[:space:]]*/, "", line)
        sub(/[[:space:]]+#.*$/, "", line)
        gsub(/^"|"$/, "", line)
        print line
        exit
      }
    }
  ' "$file")
  [ -n "$value" ] || die "$file: $section.$key is missing or empty"
  printf '%s' "$value"
}

# guest_run <vm> [NAME=value ...] <<'SCRIPT'
#
# Runs a bash script inside the guest. The script is base64-encoded so host
# quoting never reaches the guest, and it runs under `bash -euo pipefail`.
# Each NAME=value pair becomes a readonly variable at the top of the script.
# Guest stdout and stderr pass through; the guest exit status is the result.
guest_run() {
  local vm=$1
  shift
  local prelude="" assignment name value payload
  for assignment in "$@"; do
    name=${assignment%%=*}
    value=${assignment#*=}
    prelude+=$(printf 'readonly %s=%q' "$name" "$value")$'\n'
  done
  payload=$({ printf '%s' "$prelude"; cat; } | base64 | tr -d '\n')
  lume ssh "$vm" -u "$LUME_GUEST_USER" -p "$LUME_GUEST_PASSWORD" -t "$LUME_SSH_TIMEOUT" -- \
    /bin/bash -c "printf %s $payload | base64 -D | /bin/bash -euo pipefail -s"
}

# guest_state <vm> — `stopped`, `running`, or a Lume provisioning state.
guest_state() {
  lume ls --format json | jq -r --arg name "$1" '.[] | select(.name == $name) | .state' | head -n 1
}

# guest_address <vm> — the NAT address Lume assigned, empty while not running.
guest_address() {
  lume get "$1" --format json | jq -r '.[0].ipAddress // empty'
}

require_running() {
  local vm=$1 state
  state=$(guest_state "$vm")
  [ -n "$state" ] || die "no Lume VM named $vm"
  [ "$state" = "running" ] || die "$vm is $state; start it with: lume run $vm --display none"
}

# Guest credentials. Lume's unattended presets create lume/lume and the seed
# keeps them: the guest is NAT-only, and rotating the password would break
# `lume ssh`, `lume shutdown`, and `lume restart` defaults. Automation
# authenticates with the public key provision.sh installs.
: "${LUME_GUEST_USER:=lume}"
: "${LUME_GUEST_PASSWORD:=lume}"
: "${LUME_SSH_TIMEOUT:=120}"
