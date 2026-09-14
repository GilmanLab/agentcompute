# images/windows

Cluster-local Incus VM images for `windows/11/desktop` (Windows 11 Enterprise
Evaluation, Cua Driver in the interactive session, UltraVNC fallback console)
and `windows/server-2025` (Windows Server 2025 Standard Evaluation, Server
Core, Incus agent only).

These images are **never published to a registry**. Evaluation media grants
test rights, not redistribution, and it expires. Media, the repacked
installer, and the captured disk stay on the repack host and the cluster. No
Windows bytes enter git, a CI artifact, or GHCR.

| Path | Role |
| --- | --- |
| `pins.lock.yaml` | Exact product/edition/build, official media URLs and digests with their provenance, virtio-win, Cua Driver, UltraVNC, pinned servicing set, and the repack toolchain. |
| `common/instance.yaml` | Incus build and runtime settings per image: CPU, memory, disk, Secure Boot, vTPM, the agent CD, and the build-only ISO devices. |
| `common/bootstrap.ps1` | The single first-logon provisioning entry point for both images. |
| `common/finalize.ps1` | `-Phase verify` (component checks, BitLocker proof, cache purge) and `-Phase seal` (Sysprep). |
| `common/deploy-firstlogon.ps1` | Clone first-logon repair; records whether the Driver's scheduled task survived generalization. |
| `common/smoke.ps1` | In-guest clone qualification, emitting one JSON object. |
| `windows-11-desktop/` | `Autounattend.xml`, `deploy-unattend.xml`, UltraVNC setup response and server config. |
| `windows-server-2025/` | `Autounattend.xml`, `deploy-unattend.xml` selecting SERVERSTANDARDCORE. |
| `repack.py` | `validate`, `stage` (verify downloads, inject drivers, build the payload ISO), `import` (ISO volumes into the build project). |
| `bake.py` | `project`, `bake` (install, seal, publish, qualify a clone, copy a candidate alias), `promote`, `teardown`. |
| `../../.github/workflows/windows-images.yml` | Public validation plus serialized dispatch to the private bake. |

## Media provenance

`repack.py validate` refuses a lock file that claims `published` provenance
without naming a publisher, or clears a gate on a `measured` digest without
recording why.

| Image | Digest provenance |
| --- | --- |
| `windows-11-desktop` | **Published.** Microsoft's Verify Download document for Windows 11 Enterprise 25H2 lists the exact SHA-256 of the en-US evaluation ISO; the staged file matches it. |
| `windows-server-2025` | **Measured, owner-approved.** Microsoft publishes no checksum for Server evaluation media. The digest in the lock file was measured from the official HTTPS download of build 26100.32230 and approved by the owner for that exact build. It is not a Microsoft-published value, and another build needs a fresh approval. |

virtio-win is a third case: Fedora publishes an MD5 for the RPM only, and that
RPM carries no PGP signature at all, so `stage` verifies the published MD5,
extracts the ISO from the verified RPM, and checks the ISO against the
SHA-256 recorded in the lock file. The MD5 is a corruption check, not
integrity authentication.

## Two phases, two hosts

Repack needs root and loop devices but no KVM. Baking needs KVM but no root.

```sh
# 1. On a root host with scratch space (sandbox01), not inside a runner VM:
sudo env PATH="$PATH" uv run --locked --script images/windows/repack.py stage \
  --image windows-11-desktop \
  --work-dir /var/tmp/windows-bake/w11-work \
  --output-dir /var/tmp/windows-bake/w11-out \
  --cache-dir /var/tmp/windows-bake/downloads

# 2. Import the two ISOs into the disposable build project:
uv run --locked --script images/windows/bake.py project \
  --remote nas01 --project ac-win-bake-019 --evidence-dir /tmp/windows-bake-evidence
uv run --locked --script images/windows/repack.py import \
  --image windows-11-desktop --output-dir /var/tmp/windows-bake/w11-out \
  --remote nas01 --project ac-win-bake-019 --pool data --target lab01 \
  --volume-prefix w11

# 3. Bake on the cluster. This only calls the Incus API.
uv run --locked --script images/windows/bake.py bake \
  --image windows-11-desktop --remote nas01 --project ac-win-bake-019 \
  --target lab01 --pool data --volume-prefix w11 \
  --evidence-dir /tmp/windows-bake-evidence
```

`stage` requires `wimtools`, `libwin-hivex-perl`, `genisoimage`, `rsync`,
`rpm2cpio` and `cpio`; the versions used are recorded in `pins.lock.yaml`.

### The no-prompt boot image

distrobuilder 3.3.1 emits its repacked ISO with
`efi/microsoft/boot/efisys.bin`, the UEFI boot image that asks the operator to
press a key. In an unattended bake nobody does, so the firmware logs
`failed to start Boot0002 "UEFI QEMU QEMU CD-ROM": Time out` and falls through
to PXE. Microsoft ships `efisys_noprompt.bin` on the same media for this case,
so `stage` re-emits the driver-injected tree with that boot image and records
both the driver-injected and the final digest. Everything else about the ISO,
including the El Torito arguments, matches distrobuilder's own genisoimage
branch.

## Guest contract

The payload ISO carries `Autounattend.xml` at its root, where Windows Setup
finds it, and `\agentcompute` with the provisioning scripts, a `config.json`
rendered from the lock file, and the hash-pinned installers. The guest never
reaches the network during provisioning.

One `FirstLogonCommands` entry runs `bootstrap.ps1`. Microsoft starts all such
commands concurrently despite the `SynchronousCommand` name, so ordering lives
inside that one script: state directory and ACL, Incus agent from the agent
CD, automatic updates off and device encryption refused, pinned servicing, and
then, on the desktop image only, the Cua Driver and UltraVNC.

Because that script runs in the automation user's interactive logon, it is the
only place `cua-driver autostart enable` can register a task that lands in
Session 1+; a daemon in Session 0 has no desktop and enumerates no windows.

The image carries **no reusable secret**. The automation account has an empty
password, which still allows console auto-logon while Windows refuses empty
passwords for network logons by default, and UltraVNC runs with
`AuthRequired=0` behind an inbound firewall rule limited to the sandbox
subnet — the same posture the Linux desktop image uses for X0tigervnc.
`repack.py stage --automation-password` can render a password into the answer
files at bake time if that posture ever has to change; nothing is committed.

`C:\ProgramData\agentcompute` is created with an explicit ACL granting the
automation account Modify and SYSTEM Full Control, so the Session-1 Driver
daemon can write a screenshot there and the SYSTEM-side agent can read and
delete it.

## Capture and promotion

`bake.py bake` detaches both ISO volumes, verifies components, proves the OS
volume is fully decrypted (`manage-bde -status`, decrypting first if 24H2
device encryption ever turns itself on), purges cached answer files and temp
caches, then runs
`Sysprep /generalize /oobe /mode:vm /shutdown /quiet /unattend:C:\image\deploy-unattend.xml`.
The generalized source is never booted again: it is published while stopped
and then torn down with the project.

Promotion is deliberately two steps:

1. `bake` publishes inside the disposable project, qualifies a fresh clone
   (fresh machine SID and computer name, `cmd.exe /c ver`, Driver daemon in
   Session 1+, `list_windows` through the named pipe, VNC answering with
   nobody logged on), deletes the clone, and copies the image into
   `image-build` under `windows/11/desktop-candidate-<build>`.
2. `bake.py promote` moves the stable alias, and only with a GUI gate evidence
   file whose `status` is `pass`. Rollback is an alias move back to the
   previous fingerprint, printed by that command.

`bake.py teardown --delete-project` removes the instances, images, ISO volumes
and the project.
