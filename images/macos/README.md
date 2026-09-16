# macOS seed on Lume

A stopped Lume VM on one Apple Silicon host, carrying macOS, the pinned Cua
Driver, and an operator's one-time Accessibility and Screen Recording consent.
Workers are `lume clone` copies of it. It is never published to a registry:
Apple's license grants no redistribution, and a consented seed is a
desktop-control credential.

`provision.sh`, `verify.sh`, and `lib.sh` are shared by every train; each train
directory holds only `image.yaml` and `unattended.yaml`.

## Seed qualification

`macos/tahoe/desktop` exists and passes the gate on `studio-1` (Mac Studio
M2 Max, 64 GB, macOS 26.6.2 / 25G83). The host is the owner's amendment to
design prerequisite 5: an always-on Mac Studio rather than a dedicated Mac
mini, with the backend confined to a dedicated `agentcompute` account.
Migrating to a mini is moving that account's `~/.lume` directory and
re-issuing one SSH key.

First-boot clones require the identity-pinning step below. The clone gate now
rejects Setup Assistant rather than skipping the desktop check. The seed and
deployed MCP backend are qualified; see [Verify the backend](#verify-the-backend).

`macos/sequoia/desktop` is recorded but **not** the qualified train. Sequoia's
last full restore image is 15.6.1 (24G90, 2025-08-20) — a year behind that
train's security level — and its unattended install behaves no better than
Tahoe's (both leave Setup Assistant running; see Findings). Tahoe 26.6.2 is
current, matches the host's own build, and has a publisher-independent digest.

## Host layout

| Piece | Where |
| --- | --- |
| Backend Lume | `/Users/agentcompute/bin/lume`; account-local source build and installed SHA-256 in [`pins/lume.yaml`](../../pins/lume.yaml) |
| Backend account | `agentcompute`, standard (not admin), hidden, member of `com.apple.access_ssh` |
| VM store | `/Users/agentcompute/.lume` |
| API | `/Users/agentcompute/bin/lume serve --port 7777`, `/Library/LaunchDaemons/io.gilman.agentcompute.lume-serve.plist`, bound to `127.0.0.1` only |
| Approved server key policy | `from="<server-tailnet-ip>",no-agent-forwarding,no-X11-forwarding`; normal shell and local TCP forwarding |
| Root-owned account policy | `/etc/ssh/sshd_config.d/110-agentcompute.conf` (`Match User agentcompute`) |
| Guest key | Per-image key on the server; the seed's provisioned key is `/Users/agentcompute/.ssh/guest_ed25519` on Studio |

The dedicated account is the containment boundary, not a forwarding-only key.
It must remain hidden, standard, and unable to use sudo. The owner's home must
deny traversal by this account; a `staff`-readable home is insufficient because
both accounts belong to `staff`.

Phase 9a installed this policy for `agentcompute01` (`100.65.152.20`,
`agentcompute01.tailda715.ts.net`). Qualification confirmed that its private key
matches Studio's installed public key and that both source restrictions name
that address. Phase 9a proved same-key refusal from a different source.
Studio remains untagged; the minimal tailnet policy is
[networking#22](https://github.com/GilmanLab/networking/pull/22), not the older
`tag:macbackend` proposal. Qualification changed no Mac permissions or ACLs.

## Authorize the remote transport

First verify the agentcompute server VM's tailnet IPv4 address. Restrict its
public key in `~agentcompute/.ssh/authorized_keys`:

```text
from="<server-tailnet-ip>",no-agent-forwarding,no-X11-forwarding ssh-ed25519 <public-key> agentcompute server
```

Replace the forwarding-only root drop-in with the following policy, substituting
that same verified address. Keep it owned by `root:wheel`, mode `0644`.

```text
Match User agentcompute
    AllowUsers agentcompute@<server-tailnet-ip>
    AuthenticationMethods publickey
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    AllowTcpForwarding local
    PermitTunnel no
    X11Forwarding no
    AllowAgentForwarding no
    GatewayPorts no
Match all
```

Remove `ForceCommand` and `PermitOpen`; do not retain `restrict` on the key.
The root-owned source and forwarding restrictions prevent the account from
widening its access by editing `authorized_keys`. Password and keyboard-interactive
authentication must stay disabled for this account.

Before opening a new connection, run `sudo sshd -t` and inspect
`sudo sshd -T -C user=agentcompute,host=studio-1,addr=<server-tailnet-ip>`.
Keep an operator session available during the change. From the server, verify
normal shell access and that `sudo -n true` fails. Check that the account cannot
read or traverse subdirectories of `/Users/josh`; on Studio the home directory
is owner-only (`0700`). Attempt authentication with the **same key** from a
different source address and require refusal. A failure caused by the tailnet
ACL alone does not prove the SSH source restriction.

Lifecycle remains HTTP over the SSH connection to `127.0.0.1:7777`. Guest exec
and file transfer use system SSH through `ssh -J agentcompute@studio-1` to
the NAT lease, with a separate per-image guest key. Files use the SFTP subsystem
over that connection. Verify both SSH host keys;
do not forward an agent or install private keys in a guest. Host metadata uses
same-directory temporary files followed by `mv`; identity pinning edits a
stopped clone's configuration before its first run.

Lume 0.5.3 has no HTTP routes for guest exec, sidecar I/O, or setting a clone's
machine identifier. Its clone request always regenerates the identifier.

Source: Lume 0.5.3's [HTTP routes](https://github.com/trycua/cua/blob/754eec754991e1760100621e9bfe7ec1395cc7db/libs/lume/src/Server/Server.swift#L203-L401),
[clone implementation](https://github.com/trycua/cua/blob/754eec754991e1760100621e9bfe7ec1395cc7db/libs/lume/src/LumeController.swift#L296-L386),
and [host-status handler](https://github.com/trycua/cua/blob/754eec754991e1760100621e9bfe7ec1395cc7db/libs/lume/src/Server/Handlers.swift#L901-L940).

## Configure the server

Add this section to the server's existing Incus configuration. Use Studio's
tailnet **IPv4** address so the connection uses the source family authorized
above; do not use either LAN address. Replace the example credential paths
with files owned by the server's service account.

```yaml
lume:
  host: <studio-tailnet-ipv4>
  identity_file: /etc/agentcompute/ssh/studio_ed25519
  known_hosts_file: /etc/agentcompute/ssh/known_hosts
  guest_known_hosts_file: /etc/agentcompute/ssh/guest_known_hosts
  guest_keys:
    macos/tahoe/desktop: /etc/agentcompute/ssh/tahoe_ed25519
```

Private keys must be readable only by the service account. The host key and
guest key are distinct. Key-map entries use catalog image names, not VM names.
Relative paths resolve beside the server configuration file.

Verify Studio's SSH host key through operator access, and the guest host key
through the qualified seed. The host known-hosts file identifies Studio's
tailnet address. The guest known-hosts file identifies the **seed name**:

```text
ac-seed-macos-tahoe-desktop ssh-ed25519 <verified-seed-host-key>
```

Clones retain that SSH host key while their NAT leases change. Do not enroll
each new lease with `StrictHostKeyChecking=no` or trust an unverified
`ssh-keyscan` result. The generated jump configuration selects the host and
guest identities separately; neither needs an SSH agent.

The catalog's `seed:` value refers to the stopped VM in this account's Lume
store. Mac entries bypass Incus image reconciliation. Omitting the `lume`
section leaves the Incus-only runtime available.

Keep Lume's LaunchDaemon stdout and stderr at
`/Users/agentcompute/lume-serve.out.log` and
`/Users/agentcompute/lume-serve.err.log`. The capacity failure check reads only
new log bytes from the attempted run. Keep sandbox sidecars under
`/Users/agentcompute/.agentcompute/sandboxes`; do not discard them when
restarting or upgrading the server, since they are also the reaper's ownership
records.

## Verify the backend

Local checks pass with Go 1.26.6: repository lint, `go test -race ./...`,
`go build ./...`, a Linux/amd64 build with `CGO_ENABLED=0`, and the compiled
artifact's offline startup smoke check. Transport tests use real system SSH
with ProxyJump and a native SFTP server; the Lume HTTP service is a fixture.
These checks do not qualify a deployed MCP service.

After installing and proving the source-restricted SSH policy, run the live
backend lane from the authorized server VM. Set `STUDIO_TAILNET_IP` to Studio's
verified tailnet IPv4 address and adjust the credential paths to match its
configuration:

```sh
AGENTCOMPUTE_TEST_LUME=1 \
AGENTCOMPUTE_TEST_LUME_HOST="$STUDIO_TAILNET_IP" \
AGENTCOMPUTE_TEST_LUME_IDENTITY_FILE=/etc/agentcompute/ssh/studio_ed25519 \
AGENTCOMPUTE_TEST_LUME_KNOWN_HOSTS=/etc/agentcompute/ssh/known_hosts \
AGENTCOMPUTE_TEST_LUME_GUEST_KNOWN_HOSTS=/etc/agentcompute/ssh/guest_known_hosts \
AGENTCOMPUTE_TEST_LUME_GUEST_KEY=/etc/agentcompute/ssh/tahoe_ed25519 \
go test ./internal/lume -run '^TestLiveLumeLifecycle$' -count=1 -timeout=25m
```

The lane creates a uniquely named sandbox and guest, waits for desktop/Driver
readiness, checks guest execution, and deletes the sandbox on cleanup. It is
a backend API check, not a substitute for deployed MCP acceptance.

Live qualification ran from `agentcompute01` on 2026-09-16 UTC, using the
existing source-pinned key and a build based on v0.1.1. The final acceptance
build identifies source commit `4f7024d`. It retains the release's bearer
authentication and loopback reverse-proxy fixes.

MCP checks passed: Mac sandbox creation; Running guests without Setup
Assistant on the fetched screenshot; `sw_vers`; Driver `list_apps`; screenshot
URL retrieval from the agent workstation; snapshot restore recovering a
pre-snapshot file; two running guests and third-guest refusal; unsupported
network creation; restart rediscovery; and explicit sandbox deletion leaving
only the stopped seed. A running TTL guest survived a server restart and was
observed deleted 17.61 s after expiry. The final backend integration lane
passed in 74.58 s.

The final complete MCP run measured 37.46 s and 51.05 s for the two creates,
and 4.74 s for screenshot creation plus HTTPS retrieval. These are individual
observations, not cold-start guarantees: earlier creates took 80–98 s.
Machine-readable evidence is in
[`spikes/lume/qualification.json`](../../spikes/lume/qualification.json).

Qualification used a runtime-only systemd override, then restored the
fleet-pinned v0.1.1 binary, configuration, and catalog. Temporary credentials
and binaries were removed. This is qualification evidence, not a permanent
Lume rollout. PF and tailnet ACLs were not changed. Phase 9b replaces the
proposed high-port firewall gate with disabled VNC at its source, below.

Three live-only defects were fixed and regression-tested: SSH negotiated an
unpinned host-key algorithm; Lume rejected an equal-size disk PATCH; and zsh
rejected an empty sidecar glob after deletion. The backend now negotiates from
known-host pins, omits unchanged disk sizes, and executes host scripts under
`/bin/sh`.

One concurrent-listing finding remains outside this qualification fix:
`sandbox.list` can return a transient not-found error if another process deletes
a listed sandbox before its details are read. This occurred during parallel
live-lane teardown; the sequential lifecycle and TTL checks passed.

## Disable VNC before serving workers

Released Lume 0.5.3 opens a wildcard VNC listener even with `--display none`.
The owner rejected a high-port PF block because it would also affect
Continuity and `rapportd`. The obsolete anchor is removed; do not install it.

The backend instead requires the account-local source build in
[`pins/lume.yaml`](../../pins/lume.yaml), which includes upstream
[cua#3209](https://github.com/trycua/cua/pull/3209). Every run sends
`{"noDisplay":true,"vnc":"disabled"}`. Startup checks both the CLI and daemon
and fails closed if either lacks the policy. Inventory must report no VNC URL.
The global 0.5.3 install remains the seed qualification/release rollback
reference, not an automatic fallback for backend guests.

`build-lume.sh` runs only as the standard `agentcompute` account and refuses
to install a binary whose SHA-256 differs from the pin. Its source commit and
dependencies are pinned, but a measured clean rebuild is **not bitwise
reproducible**; a deliberate rebuild needs a reviewed artifact re-pin.
The version string remains 0.5.3, so verify the installed path and hash.

The canonical [agentcompute runbook](https://github.com/GilmanLab/root/blob/master/docs/docs/runbooks/agentcompute.md)
owns daemon activation, listener verification from request through Running,
credential delivery, and manual console fallback. No PF or Internet Sharing
change is required. Return to a release pin when upstream releases the
disabled-VNC feature; the pin records that revisit condition.

## Build a seed

Run the seed procedures on the Mac from a checkout readable by `agentcompute`,
for example under `/Users/agentcompute/src/agentcompute`, not the owner's
protected home. All relative paths below refer to that checkout. The pin reader
is the recipe's own, so a command cannot disagree with `pins.lock.yaml`:

```sh
pins=images/macos/pins.lock.yaml
read_pin() { ( . images/macos/lib.sh; pin "$pins" "$1" "$2" ); }
```

### 1. Install the pinned Lume

Follow `images/macos/build-lume.sh`'s staging instructions so the service
account can read the script without access to the owner's home. Run the build
as `agentcompute`, never root. The build touches only that account's home.

```sh
sudo -u agentcompute -H /tmp/lume-build/images/macos/build-lume.sh
export PATH=/Users/agentcompute/bin:$PATH
lume run --help
```

Require the distinct `--vnc <vnc>` option, not just `--vnc-port`. Repoint the
account's daemon using the canonical runbook before cloning a worker. The
released archive remains pinned in `images/macos/pins.lock.yaml` as the
historical seed-build input and in `pins/lume.yaml` as a rollback reference.

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

All ten checks must pass (6 s measured). `--identity` exercises an independent
SCP pull of the screenshot; the backend uses SFTP over the same SSH jump path.
Without it that leg is skipped and the run is not a full gate. Reboot the seed
once and re-run it before declaring the seed done, then
stop the VM and leave it stopped.

### 8. Clone smoke

```sh
curl -sS -X POST http://127.0.0.1:7777/lume/vms/clone -H 'Content-Type: application/json' \
  -d '{"name":"ac-seed-macos-tahoe-desktop","newName":"ac-smoke-1"}'
```

Before the clone's **first boot**, copy only `machineIdentifier` from the
stopped seed. Keep the clone's generated MAC; copying the MAC would create a
network collision. Run this as the `agentcompute` account, either locally or
through its source-restricted SSH shell:

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
  -d '{"noDisplay":true,"vnc":"disabled"}'
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

A subsequent disposable-clone investigation found a second seed LaunchAgent,
`io.gilman.agentcompute.cua-driver`, which runs `open -g -a`. The canonical job
sometimes exits with `daemon already running`, while the daemon belongs to an
`application.com.trycua.driver.*` launchd job. Removing the stale launcher from
that clone and rebooting did not eliminate this behavior. Both grants remained
true after kickstart. The cold-boot scheduling cause remains unresolved; this
experiment did not modify the seed or grant new permissions.

## Host-wide guest-count rule

Before a create/start request makes any Lume HTTP call, inspect the dedicated
account's inventory with `/Users/agentcompute/bin/lume ls --format json`. Count running
**macOS** guests regardless of name, including a running seed, and serialize
pending starts. Refuse a third Lume-owned running macOS guest:

```text
macOS guest limit reached (2 per host); the owner's macOS VMs share this budget
```

This inventory is not a host-wide authority: another account's VM is invisible
to it. Apple enforces the host-wide limit. If HTTP accepts a run but the VM
stays stopped and the daemon's new log output reports the limit, return an
explicit error naming Apple's two-guest limit and the possibility that another
account holds a slot. Do not mistake an old log entry for this run's failure,
or report Running from HTTP's `202` response alone.

Start and restart require the same gate. Reaper rediscovery must not start a
stopped guest merely because its sidecar exists.
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
