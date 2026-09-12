# images

Lab-built Incus images for agentcompute: the Alpine `router` system container
and two minimal Ubuntu 24.04 runner VMs. `runner` has no sudo grant;
`runner-publisher` permits only the root-owned image-build wrapper.

| Path | Role |
| --- | --- |
| `pins.yaml` | Checksummed build tools, Alpine package closure, Ubuntu base and CA bootstrap package, fixed Ubuntu snapshot, Actions Runner, and upstream guest files. |
| `router/distrobuilder.yaml` | The recipe. Installs only the pinned APKs from an offline seed (`--no-network`, empty `/etc/apk/repositories`), enables OpenRC `lxc` mode, and emits a unified tarball. |
| `runner/distrobuilder.yaml` | Split VM recipe with `runner` and `publisher` variants, signed shim/GRUB, growroot, incus-agent generator, and `ttyS0` output. |
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

For a runner VM, add `--image runner` or `--image runner-publisher`. Install
the VM assembly tools (`btrfs`, `qemu-img`, `sgdisk`, `mkfs.vfat`, `mkfs.ext4`,
`resize2fs`, `losetup`, `mount`, `rsync`, `blkid`, and `dpkg-deb`) first.
Distrobuilder requires `btrfs` even for an ext4 image; the publisher variant
bakes `btrfs-progs` alongside its other assembly tools. Output is `incus.tar.xz`,
`disk.qcow2`, and `metrics.json`; no nested virtualization is used.

The guest files are copied byte-for-byte from the pinned incus-gh-runner
v2.0.0 release and checked during validation. General runners inherit
`NoNewPrivileges=true`. The publisher omits that restriction so its sole
sudo command can run:

```sudoers
actions-runner ALL=(root) NOPASSWD: /usr/local/sbin/agentcompute-build
```

The wrapper accepts only a full lowercase commit SHA and one of the three
image names. It discards job-supplied environment variables, uses a root-owned
checkout, and refuses source outside public `origin/master` ancestry. It
does not grant direct sudo access to shells, mount tools, or `qemu-img`.

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
