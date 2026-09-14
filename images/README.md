# images

Lab-built Incus images for agentcompute: the Alpine `router` system container,
two minimal Ubuntu 24.04 runner VMs, and an Ubuntu 24.04 Xorg desktop VM.
`runner` has no sudo grant; `runner-publisher` permits only the root-owned
image-build wrapper.

| Path | Role |
| --- | --- |
| `pins.yaml` | Checksummed build tools, Alpine package closure, Ubuntu base and CA bootstrap package, fixed Ubuntu snapshot, Actions Runner, Cua Driver, and upstream guest files. |
| `router/distrobuilder.yaml` | The recipe. Installs only the pinned APKs from an offline seed (`--no-network`, empty `/etc/apk/repositories`), enables OpenRC `lxc` mode, and emits a unified tarball. |
| `runner/distrobuilder.yaml` | Split VM recipe with `runner` and `publisher` variants, signed shim/GRUB, growroot, incus-agent generator, and `ttyS0` output. |
| `ubuntu-24.04-desktop/distrobuilder.yaml` | Split desktop VM recipe with Xorg, GDM automatic login, AT-SPI, Cua Driver, X0tigervnc, systemd-networkd, growroot, and signed shim/GRUB. |
| `ubuntu-24.04-desktop/smoke.py` | Candidate qualification for X11, the Driver session socket and user unit, native `list_apps`, disabled nesting, and owned-resource cleanup. |
| `build.py` | `validate` (schema and pin checks, no credentials) and `build` (download-verify, compile distrobuilder from vendored source, assemble). PEP 723 script with `build.py.lock`. |
| `catalog.yaml` | Startup catalog: image name → digest-pinned GHCR reference or upstream Incus `remote:alias`, kind, OS, defaults. |
| `smoke.sh` | Shared six-tool router boot smoke used by image CI. |
| `runner/smoke.py` | Guest-contract qualification: disk growth, bogus payload, status transitions, serial lifecycle, poweroff, and owned-resource cleanup. |
| `publish.py` | Protected-source gate, immutable-tag lookup, assembly, qualification, publication, and verified fetch-back. |
| `catalog-pr.py` | Opens the public catalog digest PR from verified release evidence. |
| `ci-incus.py` | Configures the pinned Incus CLI and restricted HTTPS identity. |
| `../cmd/image-publish` | Immutable imgoci publication and verified fetch-back CLI. |

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

See the [Phase 6 desktop spike report](../spikes/desktop/README.md) for direct
Driver results, timing, screenshot scaling, VNC, and reboot evidence.

## Publication and import

The public `images-publish.yml` validates on a GitHub-hosted runner and
dispatches an exact SHA to private `GilmanLab/agentcompute-images`. Its
`agentcompute-publisher` scale set performs the builds, boot-tests candidates
in the restricted `image-build` project, publishes to the existing
`ghcr.io/gilmanlab/agentcompute/<image>` namespace, and fetches each digest
back independently before opening a public catalog PR.

The immutable tag hashes definition inputs under `images/`, excluding
`catalog.yaml` and Markdown. An existing release skips assembly and
publication but is still fetched and boot-qualified. Registry or
authentication errors fail the job rather than being treated as absent tags.

The previous hosted pipeline's `attest.yml` attestation step is **not**
carried into the private bake. Older releases can retain their original
attestations; new private bakes must not be described as carrying that
provenance. Digest verification and boot tests establish different properties.

The server's startup reconciler imports catalog digest references into `image-build`, verifies bytes, smoke-launches the image, and moves the alias only after success. It records the imgoci digest in image properties; the Incus fingerprint is derived, not a stable identity across rebuilds.

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

Two local desktop builds used fresh work and output directories:

| Build evidence | Download | Compile | Assemble | Peak RSS | Scratch high-water | `incus.tar.xz` | `disk.qcow2` |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| First image | 5.849 s | 31.670 s | 842.017 s | 507,180 KiB | 7,625,043,968 B | 656 B | 744,611,840 B |
| Corrected networkd rebuild | 5.824 s | 31.834 s | 392.512 s | 517,712 KiB | 7,621,808,128 B | 640 B | 745,013,248 B |

Download includes verified retrieval of Go, vendored distrobuilder source,
Ubuntu base, the snapshot CA package, and the full Cua Driver archive. Compile
excludes download and assembly. Scratch was sampled every 100 ms.

The direct token and reboot results in the spike report came from the first
image after repairing that guest's netplan in place. The corrected rebuild
subsequently passed `images/ubuntu-24.04-desktop/smoke.py` in `image-build`:
X11, the graphical user's active Driver service, and 234 native `list_apps`
entries. The smoke removed its own VM and imported image.

Protected bootstrap PR #23 and private bake run 34854323245 completed
successfully. The desktop-aware publisher must be deployed before the
four-image bake. Desktop publication, verified fetch-back, and the public
catalog update remain pending; no desktop GHCR digest is claimed.

The corrected image passed the complete production-stdio MCP acceptance run
after OVN recovery: private-only client, desktop readiness, native `list_apps`,
PNG URL fetch/decode, one foreground token click changing one editor tab to
two, reboot recovery, and the VNC endpoint reported by `desktop.info`.
The representative program took 23.624 s; the full run took 158.45 s.
The running reaper returned 404 for the original screenshot 27.120 s after
the shortened sandbox expiry. See the spike report for the native background
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
