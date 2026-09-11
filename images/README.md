# images

Lab-built Incus images for agentcompute. Phase 1 ships one: the `router`
system container (Alpine 3.22.5 with `nftables`, `frr`, `iproute2` + `tc`,
`dnsmasq`, `wireguard-tools`, `tcpdump`, nothing else).

| Path | Role |
| --- | --- |
| `pins.yaml` | Reproducibility root: distrobuilder 3.3.1 and Go 1.26.6 source URLs + SHA-256, the Alpine minirootfs, every APK in the package closure by URL + SHA-256, the Incus client used by CI, the imgoci Go module version. |
| `router/distrobuilder.yaml` | The recipe. Installs only the pinned APKs from an offline seed (`--no-network`, empty `/etc/apk/repositories`), enables OpenRC `lxc` mode, and emits a unified tarball. |
| `build.py` | `validate` (schema and pin checks, no credentials) and `build` (download-verify, compile distrobuilder from vendored source, assemble). PEP 723 script with `build.py.lock`. |
| `catalog.yaml` | Startup catalog: image name → digest-pinned GHCR reference or upstream Incus `remote:alias`, kind, OS, defaults. |
| `smoke.sh` | Shared six-tool router boot smoke used by image CI. |
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

## Publication and import

`images-publish.yml` (protected `master` only) builds, boot-tests the tarball
in the cluster's restricted `image-build` project over the Incus API, publishes
an immutable imgoci release to `ghcr.io/gilmanlab/agentcompute/router`, fetches
it back by digest, and attests the release-index digest through the isolated
`attest.yml` workflow. The immutable tag is `tree-<12 hex>` of the `images/`
git tree, so an unchanged tree cannot be republished.

Verify a release:

```sh
gh attestation verify oci://ghcr.io/gilmanlab/agentcompute/router:<tag> \
  --repo GilmanLab/agentcompute \
  --signer-workflow GilmanLab/agentcompute/.github/workflows/attest.yml
```

The server's startup reconciler imports catalog digest references into `image-build`, verifies bytes, smoke-launches the image, and moves the alias only after success. It records the imgoci digest in image properties; the Incus fingerprint is derived, not a stable identity across rebuilds.

For a verified download without import:

```sh
go run ./cmd/image-publish fetch \
  --ref ghcr.io/gilmanlab/agentcompute/router@sha256:<digest> \
  --output /tmp/router.tar.xz
```

Upstream references such as `images:alpine/3.22` are fetched when an instance first uses them. Container creation copies lab-built images from `image-build` into the sandbox project before using a local fingerprint.

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
build only in `metadata.yaml`. A master run on an unchanged `images/` tree
stops at the publish step with `release already exists`: the immutability
guard working (run 34650303253).

## Implementation

The former image spike is absorbed into `internal/incus/reconcile.go`, `cmd/image-publish`, and `images/smoke.sh`. The hand-driven build scripts used for the first three measurements are replaced by `build.py`.
