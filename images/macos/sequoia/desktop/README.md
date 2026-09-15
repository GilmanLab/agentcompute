# macOS Sequoia desktop seed

A stopped Lume VM on one Apple Silicon host, carrying macOS, the pinned Cua
Driver, and an operator's one-time Accessibility and Screen Recording consent.
Workers are `lume clone` copies of it. It is never published to a registry:
Apple's license grants no redistribution, and a consented seed is a
desktop-control credential.

## Status: not built

This recipe has never run. Design prerequisite 5 — a dedicated Apple Silicon
machine in the lab, on the network, reachable over SSH, not a personal
workstation — is not satisfied, and the lab inventory contains no Apple
hardware. Do not run these steps on a laptop or on the Mac Studio workstation:
the consented seed is a security-relevant artifact, the backend must not
disappear when a lid closes, and a personal machine's two-guest budget is not
the lab's to spend.

Every pin in `../../pins.lock.yaml` is therefore publisher-verified only. The
IPSW digest gate is open by construction (see `gate_reason` there).

## Host prerequisites

| Requirement | Value | Source |
| --- | --- | --- |
| Architecture | Apple Silicon | Lume runs nowhere else |
| Host macOS | same or newer than the guest | Lume limits |
| Memory | 16 GB for one 8 GB guest | Lume limits |
| Free disk | 50 GB plus each clone's growth | Lume limits |
| Concurrent macOS guests | 2, host-wide, seed included | Apple's Virtualization framework |
| Host tools | `lume`, `jq` | these scripts and upstream's guide |

`lume serve` is only needed by the server, not by this recipe; the scripts
drive the CLI directly. Run it as a launchd service when the server lands, and
decide its bind address there: it listens on loopback, so reaching it from the
cluster needs an explicit bind or an SSH tunnel.

## Build the seed

All commands run on the Mac host from a checkout of this repository.

### 1. Install the pinned Lume

Both steps read the pins through the recipe's own reader, so a runbook command
can never disagree with `pins.lock.yaml`:

```sh
pins=images/macos/pins.lock.yaml
read_pin() { ( . images/macos/lib.sh; pin "$pins" "$1" "$2" ); }

curl -fsSL -o /tmp/lume.tar.gz "$(read_pin lume url)"
shasum -a 256 /tmp/lume.tar.gz   # must equal $(read_pin lume sha256)
tar xzf /tmp/lume.tar.gz -C /tmp
install -m 0755 /tmp/lume /usr/local/bin/lume
lume --version                   # must print 0.5.3
```

### 2. Fetch and gate the IPSW

The `sequoia_ipsw` pin records Apple's URL, size, and a SHA-256 that Apple does
not publish (Apple's IPSW catalog carries no digest). Measure it here:

```sh
curl -fL -o /tmp/sequoia.ipsw "$(read_pin sequoia_ipsw url)"
shasum -a 256 /tmp/sequoia.ipsw   # must equal $(read_pin sequoia_ipsw sha256)
```

A match closes the pin's gate — record it in `pins.lock.yaml` in the same
change that first builds a seed. A mismatch stops the build; it is not a
licence to update the pin.

### 3. Create the seed

```sh
lume create ac-seed-macos-sequoia-desktop \
  --ipsw /tmp/sequoia.ipsw \
  --unattended images/macos/sequoia/desktop/unattended.yaml \
  --cpu 4 --memory 8GB --disk-size 100GB --display 1440x900
```

Lume installs macOS, boots once, patches the Data volume offline (the `lume`
account, autologin, SSH, no sleep), boots again, and verifies SSH.

### 4. Qualify the first display boot

```sh
lume run ac-seed-macos-sequoia-desktop   # opens a viewer
```

Upstream still reports that the Sequoia preset can stop on Setup Assistant's
Accessibility screen at first display boot. **This is a gate, not a
formality.** A guest that shows Setup Assistant is not a seed; report it and
stop. Tahoe is upstream's verified preset and the alternative on the table.

### 5. Provision

```sh
images/macos/sequoia/desktop/provision.sh \
  --vm ac-seed-macos-sequoia-desktop \
  --key ~/.ssh/agentcompute_seed.pub
```

It asserts the guest's `sw_vers` against the recipe, installs the **public**
half of the server's key, installs the checksum-verified Driver into
`/Applications/CuaDriver.app`, refuses any signing identity other than the
pinned release one, symlinks the CLI at `/usr/local/bin/cua-driver`, loads a
LaunchAgent that starts the daemon through LaunchServices, and enables Screen
Sharing. It is idempotent and installs no secrets.

Generate that key pair on the Mac host and keep the private half there. No
private key, registry token, or source-control credential goes into a guest.

### 6. Grant consent — the human step

TCC has no supported unattended path without MDM, and Cua explicitly rejects
hand-editing `TCC.db` for a reusable seed. One operator, once, in the guest's
graphical session:

```sh
lume attach ac-seed-macos-sequoia-desktop
```

Then, in the guest's Terminal (not over SSH — SSH is not the foreground Aqua
session that owns the desktop):

```sh
/usr/local/bin/cua-driver permissions grant     # approve Accessibility and Screen Recording
/usr/local/bin/cua-driver call list_apps '{}'   # allow controlling System Events
/usr/local/bin/cua-driver call get_desktop_state '{}'   # allow direct capture
```

Approve each dialog for **CuaDriver**, then run the same three commands again:
they must complete with no dialog. From the host you can watch progress
without touching the GUI:

```sh
lume ssh ac-seed-macos-sequoia-desktop -- /usr/local/bin/cua-driver permissions status --json
```

That call is read-only and cannot raise a prompt, which is exactly why a human
is required above.

### 7. Gate the seed

```sh
images/macos/sequoia/desktop/verify.sh \
  --vm ac-seed-macos-sequoia-desktop \
  --identity ~/.ssh/agentcompute_seed
```

All checks must pass. `--identity` exercises the scp pull the server will use;
without it that leg is skipped and the run is not a full gate.

### 8. Prove the grants survive cloning

```sh
lume stop ac-seed-macos-sequoia-desktop
lume clone ac-seed-macos-sequoia-desktop ac-smoke-1
lume run ac-smoke-1 --detach --display none
images/macos/sequoia/desktop/verify.sh --vm ac-smoke-1 --identity ~/.ssh/agentcompute_seed
lume stop ac-smoke-1 && lume delete ac-smoke-1 --force
```

Repeat once. Two clone cycles that pass with no new consent, and a seed that is
still stopped and unmodified afterwards, is the decision evidence for Lume. A
clone that asks for permission again is a **NOT CONFIRMED** result for the
backend, not a step to retry by hand.

Record, per run: create wall time, clone wall time, boot-to-SSH, boot-to-driver
-ready, screenshot round trip, and host disk growth per clone.

## Why this is a manual procedure

A self-hosted `lume-builder` runner with concurrency 1 can automate steps 1–3
and 5–8. Step 6 cannot be automated and cannot be waited on usefully inside a
job: GitHub Actions has no "pause mid-job for a human" primitive, and the
nearest approximation — splitting the workflow and gating the second half on an
environment approval — parks the single Mac runner for an unbounded interval
while still holding one of the host's two guest slots. Until the host exists
and the path is proven once by hand, the automation would be scaffolding around
an unexecuted procedure.

`verify.sh` is therefore the gate, not a workflow status check: it is the same
command a future job would run, and it fails closed. Register the runner and
wrap steps 1–3 and 5–8 after the first successful manual seed, with the consent
step staying a runbook entry.

## Rules

- Never `lume push` this seed or any clone. Host-local only.
- Never copy credentials into a guest. The public key in step 5 is the only
  material that crosses the boundary.
- Two running macOS guests per host, seed included. Stop the seed before
  running two workers.
- Keep the seed stopped and immutable. Never overwrite it with a failed clone.
- `sudo lume` looks in root's home for VMs. Always run as the owning account.
