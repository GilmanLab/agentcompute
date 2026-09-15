# macOS seed on Lume

A stopped Lume VM on one Apple Silicon host, carrying macOS, the pinned Cua
Driver, and an operator's one-time Accessibility and Screen Recording consent.
Workers are `lume clone` copies of it. It is never published to a registry:
Apple's license grants no redistribution, and a consented seed is a
desktop-control credential.

`provision.sh`, `verify.sh`, and `lib.sh` are shared by every train; each train
directory holds only `image.yaml` and `unattended.yaml`.

## Status: seed qualified; remote backend blocked

`macos/tahoe/desktop` exists and passes the gate on `studio-1` (Mac Studio
M2 Max, 64 GB, macOS 26.6.2 / 25G83). The host is the owner's amendment to
design prerequisite 5: an always-on Mac Studio rather than a dedicated Mac
mini, with the backend confined to a dedicated `agentcompute` account.
Migrating to a mini is moving that account's `~/.lume` directory and
re-issuing one SSH key.

First-boot clones require the identity-pinning step below. The clone gate now
rejects Setup Assistant rather than skipping the desktop check. The local
host procedure is qualified; it is not evidence of a deployed MCP backend.

`macos/sequoia/desktop` is recorded but **not** the qualified train. Sequoia's
last full restore image is 15.6.1 (24G90, 2025-08-20) — a year behind that
train's security level — and its unattended install behaves no better than
Tahoe's (both leave Setup Assistant running; see Findings). Tahoe 26.6.2 is
current, matches the host's own build, and has a publisher-independent digest.

## Host layout

| Piece | Where |
| --- | --- |
| Lume 0.5.3 | `/usr/local/bin/lume` plus `/usr/local/bin/lume.app` (the wrapper execs the bundle) |
| Backend account | `agentcompute`, standard (not admin), hidden, member of `com.apple.access_ssh` |
| VM store | `/Users/agentcompute/.lume` |
| API | `lume serve --port 7777`, `/Library/LaunchDaemons/io.gilman.agentcompute.lume-serve.plist`, bound to `127.0.0.1` only |
| Server key | authorized only for `agentcompute`, `restrict,port-forwarding,permitopen="127.0.0.1:7777",command=/usr/local/libexec/agentcompute-no-shell` |
| Root-owned duplicate of those limits | `/etc/ssh/sshd_config.d/110-agentcompute.conf` (`Match User agentcompute`) |
| Guest key | `/Users/agentcompute/.ssh/guest_ed25519`; only the public half enters a guest |

`restrict` alone does **not** stop command execution — a key with
`restrict,port-forwarding,permitopen=…` and no forced command still runs
`ssh host <command>`. The forced command is what makes it forwarding-only, and
the `sshd_config.d` drop-in repeats every limit somewhere the account cannot
edit (it owns its own `authorized_keys`, and `lume serve` runs as it).

## Remote transport boundary

The forwarding-only server key reaches the Lume API, not the host's shell or
guest SSH. Lume 0.5.3 has no HTTP routes for guest exec/SCP, arbitrary sidecar
I/O, or setting a clone's machine identifier. Its clone request always
regenerates the identifier. The current boundary therefore cannot implement
the planned remote backend. Do not enable shell access, SFTP, guest-port
forwarding, or another forwarding destination to work around it.

The owner must approve a host-side API mechanism for those operations before
server implementation can continue. A host-global capacity authority is also
needed: `/lume/host/status` counts only running VMs visible through the daemon
account's configured storage locations, including Linux VMs. It does not
enumerate the owner's other-account macOS VMs.

Source: Lume 0.5.3's [HTTP routes](https://github.com/trycua/cua/blob/754eec754991e1760100621e9bfe7ec1395cc7db/libs/lume/src/Server/Server.swift#L203-L401),
[clone implementation](https://github.com/trycua/cua/blob/754eec754991e1760100621e9bfe7ec1395cc7db/libs/lume/src/LumeController.swift#L296-L386),
and [host-status handler](https://github.com/trycua/cua/blob/754eec754991e1760100621e9bfe7ec1395cc7db/libs/lume/src/Server/Handlers.swift#L901-L940).

## Restrict VNC before serving workers

Lume's VNC listener uses an ephemeral TCP port, not port 5900. The observed
host range is 49152–65535 (`sysctl net.inet.ip.portrange.first
net.inet.ip.portrange.last`). `pf.anchor` admits that range only through
loopback and the current Tailscale interface.

**Impact:** this range also contains non-Lume applications. Review
`sudo lsof -nP -iTCP -sTCP:LISTEN` before applying it. Do not apply it if
another application requires LAN access in this range; resolve that conflict
with the owner first. These rules were syntax-checked, not installed during
the identity experiment.

1. Stop all worker VMs through the HTTP API so no pre-existing VNC states
   survive the policy change. Leave the seed stopped.
2. Find the current Tailscale interface with
   `route -n get <online-tailnet-peer-ip>`; use its `interface` value below.
   It was `utun9` during the experiment, but the number can change.
3. Validate and install only the anchor file:

   ```sh
   tailnet_if=utun9  # replace with the observed interface
   sudo pfctl -n -D "tailnet_if=$tailnet_if" -f images/macos/pf.anchor
   sudo install -o root -g wheel -m 0644 images/macos/pf.anchor \
     /etc/pf.anchors/io.gilman.agentcompute
   ```

4. Back up `/etc/pf.conf`. Add `anchor "io.gilman.agentcompute" quick`
   immediately before its existing `anchor "com.apple/*"` filter anchor.
   Preserve every existing scrub, NAT, redirect, and Apple anchor. Compare
   the live root rules (`sudo pfctl -sr`, `sudo pfctl -sn`) with the file:
   reloading it can discard dynamically inserted root rules. If they differ,
   stop and reconcile with the owner rather than losing another service's
   rules. Check `sudo pfctl -nf /etc/pf.conf` before the maintenance-window
   reload with `sudo pfctl -f /etc/pf.conf`. Never use a global flush.
5. Load the child rules:

   ```sh
   sudo pfctl -a io.gilman.agentcompute \
     -D "tailnet_if=$tailnet_if" -f /etc/pf.anchors/io.gilman.agentcompute
   sudo pfctl -a io.gilman.agentcompute -sr
   sudo pfctl -s info
   ```

   If PF is disabled, enable it with `sudo pfctl -E` and retain its reference
   token. Never use `pfctl -d`: other host services also depend on PF.
6. Start one disposable worker. Read its actual `vncUrl` port and verify a
   fresh TCP connection from loopback and an allowed tailnet peer succeeds,
   while fresh connections to both LAN addresses fail. Also test IPv6 if a
   wildcard IPv6 listener exists. Stop the worker after checking.

The child rules must be reloaded after a reboot or Tailscale interface
change, **before** starting workers. The main anchor declaration alone does
not load them. An unattended host boot loader is not installed by this
procedure. If the interface cannot be identified or the anchor is absent,
do not serve workers.

Rollback: stop workers, then unload only this anchor with
`sudo pfctl -a io.gilman.agentcompute -F rules`. Release only the PF reference
token acquired by this procedure with `sudo pfctl -X <token>`. Restore the
saved main configuration if its declaration must also be removed.

Upstream bind-address request:
[trycua/cua#3878](https://github.com/trycua/cua/issues/3878).

## Build a seed

All commands run on the Mac host from a checkout of this repository. The pin
reader is the recipe's own, so a runbook command cannot disagree with
`pins.lock.yaml`:

```sh
pins=images/macos/pins.lock.yaml
read_pin() { ( . images/macos/lib.sh; pin "$pins" "$1" "$2" ); }
```

### 1. Install the pinned Lume

```sh
curl -fsSL -o /tmp/lume.tar.gz "$(read_pin lume url)"
shasum -a 256 /tmp/lume.tar.gz          # must equal $(read_pin lume sha256)
tar xzf /tmp/lume.tar.gz -C /tmp
sudo ditto /tmp/lume.app /usr/local/bin/lume.app
sudo install -m 0755 /tmp/lume /usr/local/bin/lume
lume --version                          # 0.5.3
sudo -u agentcompute -H lume config telemetry disable
```

The archive ships a shell wrapper plus `lume.app`; both must land in the same
directory or the wrapper cannot find the binary.

### 2. Fetch and verify the IPSW

```sh
sudo -u agentcompute curl -fL -o /Users/agentcompute/ipsw/UniversalMac_26.6.2_25G83_Restore.ipsw \
  "$(read_pin tahoe_ipsw url)"
shasum -a 256 /Users/agentcompute/ipsw/UniversalMac_26.6.2_25G83_Restore.ipsw
```

Apple publishes no digest for restore images, so the pin carries AppleDB's
value and the `gate` field records that it was measured locally. Measured here:
212 s to download 19.7 GB, digest matched.

### 3. Create the seed

```sh
sudo -u agentcompute -H lume create ac-seed-macos-tahoe-desktop \
  --ipsw /Users/agentcompute/ipsw/UniversalMac_26.6.2_25G83_Restore.ipsw \
  --unattended images/macos/tahoe/desktop/unattended.yaml \
  --cpu 4 --memory 8GB --disk-size 100GB --display 1920x1200
```

287 s measured. Lume installs macOS, boots once, patches the Data volume
offline (the `lume` account, autologin, SSH, no sleep), boots again, verifies
SSH, and stops the VM.

`--display 1920x1200` is not cosmetic: at 1440x900 macOS clips Setup
Assistant's button row off-screen, which makes the console pass below
impossible.

### 4. Console pass — completes Setup Assistant once

Lume's offline setup does **not** remove Setup Assistant. The guest boots with
autologin working, Finder and Dock running, and MiniBuddy full-screen on top.
Start the VM and drive its console:

```sh
curl -sS -X POST http://127.0.0.1:7777/lume/vms/ac-seed-macos-tahoe-desktop/run \
  -H 'Content-Type: application/json' -d '{"noDisplay":false}'
lume get ac-seed-macos-tahoe-desktop --format json | jq -r '.[0].vncUrl'
```

Open that `vnc://` URL with Screen Sharing, or drive it headlessly with
`vncdotool` (`uv run --with vncdotool`). Work through the remaining screens:
automatic updates → Continue; Apple Account → Other Sign-In Options → Sign in
Later in Settings → Skip; Age Range → Adult; FileVault → Cancel the password
prompt, Not Now, Continue. Do **not** enable FileVault: Lume cannot resize a
FileVault disk and the seed must stay cloneable.

Never `killall "Setup Assistant"`. Measured: it logs the session out to the
login window, with or without Finder running.

### 5. Provision

```sh
sudo -u agentcompute -H images/macos/provision.sh \
  --vm ac-seed-macos-tahoe-desktop \
  --key /Users/agentcompute/.ssh/guest_ed25519.pub \
  --recipe images/macos/tahoe/desktop/image.yaml
```

6 s measured, idempotent. It asserts `sw_vers` against the recipe, installs the
public key, installs the checksum-verified Driver into
`/Applications/CuaDriver.app`, refuses any signing identity but the pinned
release one, symlinks `/usr/local/bin/cua-driver`, suppresses macOS's
first-login flow, turns off the guest's software-update schedule, and loads the
Driver LaunchAgent.

The LaunchAgent label is `com.trycua.cua_driver_daemon` and it execs
`/Applications/CuaDriver.app/Contents/MacOS/cua-driver serve` directly.
`open -g -a CuaDriver --args serve` — which upstream's troubleshooting
suggests — makes `open` the responsible process, and the Driver then refuses to
read its own TCC state (`no CuaDriver daemon is running under the driver's own
identity`) even though the process is the same binary. Under launchd the daemon
is its own responsible process and `permissions status` reports attribution
`driver-daemon`.

### 6. Grant consent — the human step

TCC has no supported unattended path without MDM, and Cua rejects editing
`TCC.db` for a reusable seed. Once, on the seed, through the console:

```sh
ssh -i /Users/agentcompute/.ssh/guest_ed25519 lume@<guest-ip> \
  /usr/local/bin/cua-driver permissions grant
```

Then, in the guest's GUI:

1. **Accessibility.** The prompt offers *Open System Settings*; in Privacy &
   Security → Accessibility enable **CuaDriver**, authenticate as `lume`.
2. **Screen Recording.** macOS does not add the app by itself. Open
   `x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture`,
   click **+**, choose `/Applications/CuaDriver.app` (select Applications in the
   sidebar and type the name), then enable its switch. Dismiss the "may not be
   able to record until it is quit" sheet with **Later** — clicking *Quit &
   Reopen* here left the switch off in testing.
3. Restart the daemon so it picks the grants up:
   `launchctl kickstart -k gui/$(id -u)/com.trycua.cua_driver_daemon`.

`cua-driver permissions status --json` must then report `accessibility: true`,
`screen_recording: true`, and `source.attribution: "driver-daemon"` with
`bundle_id: com.trycua.driver`. Until both grants are in place every tool call
returns `permissions_pending`; `permissions grant` itself times out after about
three minutes with `Timed out waiting on: Screen Recording`.

### 7. Gate the seed

```sh
sudo -u agentcompute -H images/macos/verify.sh \
  --vm ac-seed-macos-tahoe-desktop \
  --recipe images/macos/tahoe/desktop/image.yaml \
  --identity /Users/agentcompute/.ssh/guest_ed25519
```

All ten checks must pass (6 s measured). `--identity` exercises the scp pull
the server will use; without it that leg is skipped and the run is not a full
gate. Reboot the seed once and re-run it before declaring the seed done, then
stop the VM and leave it stopped.

### 8. Clone smoke

```sh
curl -sS -X POST http://127.0.0.1:7777/lume/vms/clone -H 'Content-Type: application/json' \
  -d '{"name":"ac-seed-macos-tahoe-desktop","newName":"ac-smoke-1"}'
```

Before the clone's **first boot**, copy only `machineIdentifier` from the
stopped seed. Keep the clone's generated MAC; copying the MAC would create a
network collision. Run this local host procedure as the `agentcompute`
account, not through the forwarding-only server key:

```sh
sudo -u agentcompute -H /bin/bash -euo pipefail <<'PIN'
store="$HOME/.lume"
seed="$store/ac-seed-macos-tahoe-desktop/config.json"
clone="$store/ac-smoke-1/config.json"
identifier=$(/usr/bin/plutil -extract machineIdentifier raw -o - "$seed")
tmp=$(/usr/bin/mktemp "$store/ac-smoke-1/.config.XXXXXX")
trap 'rm -f "$tmp"' EXIT
cp "$clone" "$tmp"
/usr/bin/plutil -replace machineIdentifier -string "$identifier" "$tmp"
jq -e --arg id "$identifier" '.machineIdentifier == $id' "$tmp" >/dev/null
mv -f "$tmp" "$clone"
PIN
```

The rename replaces the configuration atomically on the same filesystem.
Never run it while the clone is running. Then start through HTTP:

```sh
curl -sS -X POST http://127.0.0.1:7777/lume/vms/ac-smoke-1/run -H 'Content-Type: application/json' \
  -d '{"noDisplay":true}'
```

Wait until `GET /lume/vms/ac-smoke-1` reports a NAT address and SSH accepts
connections. Start the already-installed Driver LaunchAgent if it is pending;
this does not grant or alter TCC permissions:

```sh
ip=$(curl -fsS http://127.0.0.1:7777/lume/vms/ac-smoke-1 | jq -er .ipAddress)
sudo -u agentcompute ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
  -i /Users/agentcompute/.ssh/guest_ed25519 "lume@$ip" \
  'launchctl kickstart gui/$(id -u)/com.trycua.cua_driver_daemon'
sudo -u agentcompute -H images/macos/verify.sh --vm ac-smoke-1 --clone \
  --recipe images/macos/tahoe/desktop/image.yaml \
  --identity /Users/agentcompute/.ssh/guest_ed25519
```

The earlier unpinned measurements were clone 2–3 s, NAT address 10–11 s,
and host allocation about 26.5 GB per clone. Those runs skipped the wizard
check and are not desktop-readiness measurements.

On 2026-09-15, a fresh pre-boot-pinned clone and a second pinned clone both
ran concurrently, kept their distinct MACs, retained both TCC grants, and
passed the strict gate after the Driver kickstart. Screenshots showed the
desktop, not Setup Assistant. A fresh unpinned control still showed the
wizard: the old gate exited 0, while the corrected gate exited 1.
The comparison and gate output are retained in
[`spikes/lume/identity.json`](../../spikes/lume/identity.json).

The pinned UUID matched the seed. System loginwindow and system/user Setup
Assistant plists were identical in the initial seed/control comparison;
the clone had acquired an additional UUID-named ByHost loginwindow plist.
Changing only `machineIdentifier` restored the seed's guest UUID and serial.
No dismissal agent or TCC database modification was used. Rebooting an
already-booted unpinned clone also cleared the wizard in one experiment:
use **fresh first-boot controls**, not a reboot alone, to qualify pinning.

Both pinned and unpinned clones sometimes left the Driver LaunchAgent in
`pended nondemand spawn = speculative`, with zero runs. `launchctl kickstart`
started the existing app-owned daemon; both grants then remained true.

## Host-wide guest-count rule

Before clone/start, reserve capacity against the host's running macOS guests
and pending starts, including the owner's VMs and the seed if running. Refuse
the third before sending a mutating Lume request:

```text
macOS guest limit reached (2 per host); the owner's macOS VMs share this budget
```

Creation, restart/start, and reaper recovery need the same host-wide authority
and serialized reservations. Counting only `ac-*` names or the backend
account's `lume ls` is insufficient. Enumeration is needed to establish the
count, so “before any API call” means before any **mutating** API call, not
before the inventory read. Lume 0.5.3 supplies no authoritative cross-account
count; this gate is a server prerequisite, not implemented by these scripts.
See [trycua/cua#3880](https://github.com/trycua/cua/issues/3880).

## Measurements

| Step | Measured |
| --- | --- |
| IPSW download (19.7 GB, Tahoe) | 212 s |
| IPSW download (15.7 GB, Sequoia) | 191 s |
| `lume create` (Tahoe, 100 GB disk) | 287 s |
| `lume create` (Sequoia) | 275 s |
| Clone | 2–3 s |
| Boot to NAT address | 10–11 s |
| Boot to Driver ready with grants | ~30 s |
| `provision.sh` | 6 s |
| `verify.sh` (incl. capture + scp pull) | 6–7 s |
| Screenshot | 1.3–1.5 MB on the seed, ~0.37 MB on an idle clone |
| Seed on disk | 26.6 GB allocated of a 100 GB logical disk |

## Findings

1. **Clone identity triggers first-boot Setup Assistant.** Pinning the seed's
   `machineIdentifier` before first boot removed the wizard while preserving
   the clone's generated MAC. The strict `verify.sh --clone` gate always
   checks the desktop. See the clone smoke procedure and its control evidence.
2. **Lume's API accepts a third macOS guest and silently does nothing.**
   `POST /lume/vms/<n>/run` returns `202 {"status":"pending"}`; the VM stays
   `stopped` and only `lume serve`'s log carries `The number of virtual
   machines exceeds the limit`. Any caller must count running guests itself and
   refuse with its own error.
3. **`lume ssh` is not argv-safe.** It joins its command arguments with spaces
   and the guest re-parses the result, so `lume ssh vm /bin/bash -c '<script>'`
   arrives as `bash -c <first-word> <rest…>`. Commands must be composed as one
   valid shell line (`lib.sh` base64-encodes the payload for exactly this
   reason). Report: [trycua/cua#3879](https://github.com/trycua/cua/issues/3879).
4. **`lume run --detach` needs a controlling terminal.** From a non-PTY context
   it fails with `nohup: can't detach from console`. Start VMs through the HTTP
   API instead, which is also the server's path.
5. **Lume's per-VM VNC server listens on every interface** (`*:52397` observed)
   while the API is loopback-only. On a host that is not otherwise firewalled
   this exposes the guest console, password-protected, to the LAN. In-guest
   Screen Sharing is not the fallback: `launchctl enable
   system/com.apple.screensharing` fails on macOS 26.
6. **The Driver's identity check is strict about how the daemon starts** (see
   step 5). Upstream's own troubleshooting command produces a daemon its
   permission gate rejects.
7. **`cua-driver status | head` panics** the CLI with a broken pipe.

## Rules

- Never `lume push` the seed or a clone. Host-local only.
- Never copy credentials into a guest. The public key in step 5 is the only
  material that crosses the boundary.
- Two running macOS guests per host, the seed included. The owner's own macOS
  VMs share that budget.
- Keep the seed stopped and immutable. Never overwrite it with a failed clone.
- `sudo lume` looks in root's home for VMs. Always run as `agentcompute`.
