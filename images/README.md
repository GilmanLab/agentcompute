# images

Lab-built guest images for agentcompute: the Alpine `router` system container,
two minimal Ubuntu 24.04 runner VMs, an Ubuntu 24.04 Xorg desktop VM, the
cluster-local Windows VMs, and one non-Incus family — a host-local macOS seed
built by Lume on an Apple Silicon host.
`runner` has no sudo grant; `runner-publisher` permits only the root-owned
image-build wrapper.

| Path | Role |
| --- | --- |
| `pins.yaml` | Checksummed build tools, Alpine package closure, Ubuntu base and CA bootstrap package, fixed Ubuntu snapshot, Actions Runner, Cua Driver, and upstream guest files. |
| `router/distrobuilder.yaml` | The recipe. Installs only the pinned APKs from an offline seed (`--no-network`, empty `/etc/apk/repositories`), enables OpenRC `lxc` mode, and emits a unified tarball. |
| `runner/distrobuilder.yaml` | Split VM recipe with `runner` and `publisher` variants, signed shim/GRUB, growroot, incus-agent generator, and `ttyS0` output. |
| `ubuntu-24.04-desktop/distrobuilder.yaml` | Split desktop VM recipe with Xorg, GDM automatic login, AT-SPI, Cua Driver, X0tigervnc, systemd-networkd, growroot, and signed shim/GRUB. |
| `ubuntu-24.04-desktop/smoke.py` | Candidate qualification for X11, the Driver session socket and user unit, native `list_apps`, live whole-desktop capture after launching Text Editor, disabled nesting, and owned-resource cleanup. |
| `build.py` | `validate` (schema and pin checks, no credentials) and `build` (download-verify, compile distrobuilder from vendored source, assemble). PEP 723 script with `build.py.lock`. |
| `catalog.yaml` | Startup catalog: name → digest-pinned GHCR reference, upstream Incus `remote:alias`, or cluster-local `alias` in `image-build`, plus kind, OS, and defaults. |
| `smoke.sh` | Router boot qualification, including the four `/opt/router/{nat,route,dhcp,wg}` helpers. |
| `runner/smoke.py` | Guest-contract qualification: disk growth, bogus payload, status transitions, serial lifecycle, poweroff, and owned-resource cleanup. |
| `publish.py` | Protected-source gate, immutable-tag lookup, assembly, qualification, publication, and verified fetch-back. |
| `catalog-pr.py` | Opens the public catalog digest PR from verified release evidence. |
| `ci-incus.py` | Configures the pinned Incus CLI and restricted HTTPS identity. |
| `macos/pins.lock.yaml` | Lume, Cua Driver, and Tahoe/Sequoia IPSW pins for the host-local macOS seed, with provenance labels and locally measured digests. |
| `macos/lib.sh` | Host helpers shared by the seed scripts: the pin reader and the quoting-safe `lume ssh` transport. |
| `macos/provision.sh` | Idempotent seed provisioning: base assertions, automation public key, pinned Driver, first-login suppression, Driver LaunchAgent. |
| `macos/verify.sh` | The qualification gate: guest build, Driver identity, TCC grants held by the daemon, accessibility tree, capture plus scp pull. `--clone` for workers. |
| `macos/{tahoe,sequoia}/desktop/` | Per-train `image.yaml` and `unattended.yaml`. `tahoe` is the qualified train. |
| `macos/README.md` | Operator runbook, measurements, and findings. Produces no publishable artifact. |
| `../cmd/image-publish` | Immutable imgoci publication and verified fetch-back CLI. |

Phase 7 router and Windows findings are maintained in the central
[qualification report](https://docs.gilman.io/root/reference/agentcompute/phase7/).

## Build locally

Linux amd64, root, `tar`/`xz`/`gcc` on `PATH`, `uv` (mise provides 0.11.0):

```sh
uv run --locked --script images/build.py validate
mkdir -p /var/tmp/router-build
sudo env PATH="$PATH" uv run --locked --script images/build.py build \
  --work-dir /var/tmp/router-build/work --output-dir /var/tmp/router-build/out
```

Output: `out/router.tar.xz` and `out/metrics.json`. Both `--work-dir` and
`--output-dir` must not exist yet; their parent must. The script never
deletes anything it did not create.

distrobuilder needs root and loop devices, not KVM. macOS cannot run it;
`sandbox01` can.

For a VM, add `--image runner`, `--image runner-publisher`, or
`--image ubuntu-24.04-desktop`. Install the VM assembly tools (`btrfs`,
`qemu-img`, `sgdisk`, `mkfs.vfat`, `mkfs.ext4`, `resize2fs`, `losetup`,
`mount`, `rsync`, `blkid`, and `dpkg-deb`) first. Distrobuilder requires
`btrfs` even for an ext4 image; the publisher variant bakes `btrfs-progs`
alongside its other assembly tools. Output is `incus.tar.xz`, `disk.qcow2`,
and `metrics.json`; no nested virtualization is used.

The guest files are copied byte-for-byte from the pinned incus-gh-runner
v2.0.0 release and checked during validation. General runners inherit
`NoNewPrivileges=true`. The publisher omits that restriction so its sole
sudo command can run:

```sudoers
actions-runner ALL=(root) NOPASSWD: /usr/local/sbin/agentcompute-build
```

The wrapper accepts only a full lowercase commit SHA and one of the four
image names. It discards job-supplied environment variables, uses a root-owned
checkout, and refuses source outside public `origin/master` ancestry. It
does not grant direct sudo access to shells, mount tools, or `qemu-img`.

### Desktop image

The desktop recipe starts from the pinned Ubuntu base and snapshot
`20260911T000000Z`. It builds a 16 GiB amd64 VM with Xorg, automatic login as
`automation`, `gnome-text-editor`, AT-SPI, and X0tigervnc on guest TCP port
5900. Netplan explicitly selects `renderer: networkd`, and the recipe enables
`systemd-networkd`; this is the correction found after the first guest needed
the same setting applied in place.

Cua Driver 0.28.1 is pinned to the full
`cua-driver-rs-0.28.1-linux-x86_64.tar.gz` archive with SHA-256
`a068b6e477893b77ced74bceccf7db7483cf140e8d54150ce5849b6252b90bcf`.
The GitHub release is a prerelease accepted under the project-specific pin
exception.

The graphical session runs a persistent user daemon at
`/run/user/1000/cua-driver.sock`. Host calls use a new one-shot
`cua-driver call --socket /run/user/1000/cua-driver.sock` process for each
native tool invocation. They run as UID 1000 from `/home/automation` with
`HOME=/home/automation` and `XDG_RUNTIME_DIR=/run/user/1000`. Native tool names
and JSON arguments pass through unchanged; the host does not add typed
wrappers for individual Driver tools. Screenshots travel through the binary
guest-file API and return as URLs rather than embedded image data.

The daemon serves with `--no-overlay`: the synthetic agent-cursor overlay
freezes X root-window reads on this GNOME/Xorg session. Starting before the
first composite freezes a black frame; both Driver whole-desktop capture and
VNC are affected. Reproduced on Driver 0.28.1 and 0.28.2. Window capture and
input remain functional. Disabling the cosmetic overlay restores live capture
but removes its session-coloured pointer; re-enabling it reintroduces the defect.
The desktop smoke compares image data before and after launching Text Editor,
so it rejects a frozen frame even when that frame is not black.

See the [Phase 6 desktop spike report](../spikes/desktop/README.md) for direct
Driver results, timing, screenshot scaling, VNC, and reboot evidence.

### macOS seed on Lume

`macos/` is the one family that produces no artifact. The deliverable is a
stopped Lume VM on a dedicated Apple Silicon host, holding an operator's
one-time Accessibility and Screen Recording consent; workers are `lume clone`
copies. Apple's license grants no redistribution and Cua's guidance is to keep
a consented seed private, so it is never pushed to a registry and the catalog
would carry a `seed:` name instead of a digest or alias.

`macos/tahoe/desktop` is built and qualified on the lab's Mac Studio: clone in
2–3 s, Driver answering with both TCC grants about 30 s after boot, and no
clone ever needing consent again. `macos/sequoia/desktop` is recorded but not
qualified — Sequoia's last full restore image is a year old, and it clears no
gate that Tahoe does not.

There is still deliberately no `catalog.yaml` row: the server answers
`platform "mac" is not available yet`, and a catalog entry would advertise an
image no `instance.create` can launch. `internal/lume` is the next change, not
this one.

`images-validate.yml` checks what a Linux runner can: the scripts parse and
every pin they read still resolves. The operator procedure, its gates, the
measurements, and the open findings (a clone re-runs Setup Assistant; Lume's
API accepts a third guest and silently ignores it) are in
[`macos/README.md`](macos/README.md).

## Publication and import

The public `images-publish.yml` validates on a GitHub-hosted runner and
dispatches an exact SHA to private `GilmanLab/agentcompute-images`. Its
`agentcompute-publisher` scale set performs the builds, boot-tests candidates
in the restricted `image-build` project, publishes to the existing
`ghcr.io/gilmanlab/agentcompute/<image>` namespace, and fetches each digest
back independently before opening a public catalog PR.

A newly created GHCR package starts private. For this public catalog, its
owner must explicitly make the package public in GitHub's package settings
after the first publication. This change cannot be reversed to private.
Verify anonymous access before merging its catalog entry: authenticated
fetch-back by the publisher does not establish public availability.

The immutable tag hashes definition inputs under `images/`, excluding
`catalog.yaml` and Markdown. An existing release skips assembly and
publication but is still fetched and boot-qualified. Registry or
authentication errors fail the job rather than being treated as absent tags.

The previous hosted pipeline's `attest.yml` attestation step is **not**
carried into the private bake. Older releases can retain their original
attestations; new private bakes must not be described as carrying that
provenance. Digest verification and boot tests establish different properties.

The server's startup reconciler imports catalog digest references into `image-build`, verifies bytes, smoke-launches the image, and moves the alias only after success. It records the imgoci digest in image properties; the Incus fingerprint is derived, not a stable identity across rebuilds.
Desktop VM qualification waits for X11, the automation user's Driver
service, and a native `list_apps` call through that user's session. It uses
the catalog's CPU and memory defaults and does not require the GitHub runner.

For a verified download without import:

```sh
go run ./cmd/image-publish fetch \
  --ref ghcr.io/gilmanlab/agentcompute/router@sha256:<digest> \
  --output /tmp/router.tar.xz
```

Upstream references such as `images:alpine/3.22` are fetched when an instance first uses them. Container creation copies lab-built images from `image-build` into the sandbox project before using a local fingerprint.

### Public repository threat review

`agentcompute` is public. Fork workflow approval and a variable-held runner
label are not sufficient isolation: workflow code can request a literal
self-hosted label. No self-hosted runner is registered with this repository,
and `IMAGES_RUNNER` is removed rather than repurposed.

The deployment requires all of these controls:

- Public PR validation remains GitHub-hosted. The publisher has only
  protected-`master` push and manual-dispatch triggers, never `pull_request`
  or `pull_request_target`.
- Public Actions settings require approval for **all** outside contributors
  and full commit SHAs for actions. The protected source branch and the
  `image-publish` environment remain the dispatch gate.
- The scale set is repository-scoped to private `agentcompute-images`, in
  the `default` runner group. Confirm that binding before activation; do
  not expose it through the public repository or an organization runner group.
- A dedicated contents-write App token dispatches to the private repository.
  The private workflow and the root build wrapper independently require the
  source SHA to be an ancestor of public `origin/master`.
- A separately scoped token from that App writes only the public catalog
  branch and PR. The controller uses its own administration-write App.

Check the public boundary with:

```sh
gh api repos/GilmanLab/agentcompute/actions/permissions
gh api repos/GilmanLab/agentcompute/actions/permissions/fork-pr-contributor-approval
gh api repos/GilmanLab/agentcompute/actions/runners
gh variable list --repo GilmanLab/agentcompute
```

Trusted source maintainers and the private workflow can authorize root image
assembly. Repository scoping does not remove that trust or the supply-chain
risk of tools executed by the publisher. Runner VMs retain
`security.nesting=false` and `security.secureboot=true`; network egress and
Incus authority are restricted independently.

Deployment, credential escrow, package access, rollback, and live acceptance
checks are in the
[central runbook](https://jmgilman.github.io/root/runbooks/private-image-runners/).
Do not enable the private bake until those checks pass.

## imgoci representation decision

imgoci standardizes `incus-vm` (split `metadata` + `disk`) and has no public
value for a unified Incus container tarball. The spec reserves public values
and requires producer-defined selectors to use `x-<owner>-<name>`, so the
router release uses:

| Selector | Value |
| --- | --- |
| `io.imgoci.target` | `incus` |
| `io.imgoci.representation` | `x-gilmanlab-incus-container` |
| `io.imgoci.role` | `x-gilmanlab-unified` |
| `io.imgoci.compression` | `none` |
| `io.imgoci.architecture` | `amd64` |

Compression is `none` because the xz wrapper is part of the Incus unified
image format (`incus image import` consumes it as-is); the content digest is
the digest of `router.tar.xz`, which equals the Incus fingerprint. The image
is not labelled `incus-vm`. Proposing a public `incus-container`
representation upstream is future work.

Runner releases use the standard `incus-vm` representation, with `metadata`
and `disk` roles and `none` compression. Their Incus fingerprint is SHA-256
over the metadata bytes followed by disk bytes. The release-index digest,
not that derived fingerprint, is the catalog's durable identity.

## Measured builds

Assembly on `sandbox01` (Ubuntu 26.04, 7.0 kernel, AMD Ryzen 7 UM760), each
in a fresh work directory:

| Build | Assemble wall | Peak RSS | Scratch high-water | `router.tar.xz` | Decoded |
| --- | --- | --- | --- | --- | --- |
| 1 (hand-driven) | 0.904 s | 153,288 KiB | 159,870,976 B | 11,677,588 B | 44,462,635 B |
| 2 (hand-driven) | 0.901 s | 153,284 KiB | 159,199,232 B | 11,677,972 B | 44,462,635 B |
| 3 (hand-driven) | 0.897 s | 153,544 KiB | 169,447,424 B | 11,678,200 B | 44,462,635 B |
| `build.py` | 1.17 s | 507,696 KiB | 954,527,744 B | 11,678,052 B | 44,462,635 B |
| `build.py`, GitHub-hosted `ubuntu-24.04` (run 34649751291) | 2.03 s | 512,460 KiB | 954,454,016 B | 11,678,744 B | 44,462,635 B |
| `build.py`, GitHub-hosted `ubuntu-24.04` (run 34650677380) | 2.92 s | 514,596 KiB | 954,454,016 B | 11,678,548 B | 44,462,635 B |

`build.py` numbers include the Go toolchain and vendored distrobuilder source
in the work directory (download 7.8 s / 5.8 s, compile 31.6 s / 66.4 s on
sandbox01 / the hosted runner), which is why its RSS and scratch are higher;
the assembly phase itself is the ~1–2 s row. The whole hosted publish run
(build, boot test on the cluster, publish, fetch-back, attest) took about
3.5 minutes. An earlier
first attempt failed in 0.1 s: the minirootfs's `libssl3` pinned the older
`libcrypto3`, which is why `pins.yaml` now pins the base packages together
with the router set (59 APKs, 13.2 MB downloaded).

Scratch is sampled every 100 ms, so short peaks between samples are missed.

### Desktop Phase 6

The two local builds and protected publication produced these measurements:

| Build evidence | Download | Compile | Assemble | Peak RSS | Scratch high-water | `incus.tar.xz` | `disk.qcow2` |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| First image | 5.849 s | 31.670 s | 842.017 s | 507,180 KiB | 7,625,043,968 B | 656 B | 744,611,840 B |
| Corrected networkd rebuild | 5.824 s | 31.834 s | 392.512 s | 517,712 KiB | 7,621,808,128 B | 640 B | 745,013,248 B |
| Protected published build | 6.624 s | 33.283 s | 403.364 s | 505,216 KiB | 7,620,071,424 B | 628 B | 742,923,264 B |

Download includes verified retrieval of Go, vendored distrobuilder source,
Ubuntu base, the snapshot CA package, and the full Cua Driver archive. Compile
excludes download and assembly. Scratch was sampled every 100 ms.

The direct token and reboot results in the spike report came from the first
image after repairing that guest's netplan in place. The corrected rebuild
subsequently passed `images/ubuntu-24.04-desktop/smoke.py` in `image-build`:
X11, the graphical user's active Driver service, and 234 native `list_apps`
entries. The smoke removed its own VM and imported image.

The desktop-aware publisher is deployed. Image PR #25 merged as
`e4333f245b4e81c8d7753038f0ddf04a620bd0a2`, and
[protected bake 34872818589](https://github.com/GilmanLab/agentcompute-images/actions/runs/34872818589)
built, boot-qualified, published, and fetched back all four images.
The desktop pipeline took 674.806 s and produced:

```text
ghcr.io/gilmanlab/agentcompute/ubuntu-24.04-desktop@sha256:5dc4e120a79dd06ad6784e69474f0617387f74cb98685af8844170b7165ea8e2
```

[Catalog PR #27](https://github.com/GilmanLab/agentcompute/pull/27) records
this digest and remains unmerged by request. Published artifact hashes and
the independent publisher rollout evidence are in the spike report.

The corrected image passed the complete production-stdio MCP acceptance run
after OVN recovery: private-only client, desktop readiness, native `list_apps`,
PNG URL fetch/decode, one foreground token click changing a single document
to two document tabs, reboot recovery, and the VNC endpoint reported by
`desktop.info`. The post-refactor repeat's representative program took
22.764 s; full acceptance took 160.32 s. The running reaper returned 404 for
the original screenshot 26.804 s after the shortened sandbox expiry.
See the spike report for the native background
delivery limitation and [fleet PR #20](https://github.com/GilmanLab/fleet/pull/20)
for the separately recovered stale-CA reconnect storm and active log limits.

### Reproducibility

The three hand-driven tarballs decode to identical member sets, contents,
sizes, modes, and owners. Only `metadata.yaml` (`creation_date`,
`expiry_date`, stamped by distrobuilder at build time) and member mtimes
differ, so the compressed bytes and therefore the Incus fingerprint differ per
build. A CI build will not reproduce a local digest; compare decoded content,
not the fingerprint. Fixing the timestamps is deliberately not done in Phase 1.

Two releases exist. `tree-b2d8dff139ca` (run 34649751291) has the same
contents as the local spike build but 0600/0700 modes on files distrobuilder
generates, because `build.py` assembled under umask 077; that is fixed. The
catalog points at `tree-05771e889f6c`
(`sha256:6b3ecd8336b6fce7006e764e01373199e2cc1cf46172132b53e0aaebd27be889`,
fingerprint `6e3183fe052e…`, run 34650677380), which differs from the local
build only in `metadata.yaml`. The old pipeline failed an unchanged-tree
master run with `release already exists` (run 34650303253). The private
bake now checks the immutable tag before assembly.

## Implementation

The former image spike is absorbed into `internal/incus/reconcile.go`, `cmd/image-publish`, and `images/smoke.sh`. The hand-driven build scripts used for the first three measurements are replaced by `build.py`.
