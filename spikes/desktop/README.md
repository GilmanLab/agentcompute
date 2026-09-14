# Desktop Phase 6 spike

**2026-09-14 — direct native probing passed on a repaired first-build guest, and the corrected rebuild passed its X11/Driver boot smoke. Live MCP acceptance is blocked by OVN infrastructure.** The native probe exercised Cua Driver 0.28.1 through its session socket, including a token-addressed GUI change, bounded screenshots, VNC, and restart recovery.

## Evidence and scope

The results use these evidence sources:

| Evidence | What it establishes |
| --- | --- |
| `/tmp/agentcompute-desktop-phase6-image/metrics.json` | First local image build measurements and artifact hashes. |
| `/tmp/agentcompute-desktop-spike/` | Direct native Driver results from a guest booted from the first build after its netplan was repaired in place. Each operation has a report, stdout, stderr, and any PNG pulled from the guest. `retained-evidence.json` preserves the timing comparison, status checks, native 640-pixel capture, VNC checks, and restart timing. |
| `/tmp/agentcompute-desktop-phase6-final/metrics.json` | Corrected local rebuild measurements and artifact hashes. It is build evidence, not live Driver or MCP acceptance evidence. |
| `internal/desktop/testdata/` | Captured Driver catalog and result fixtures used by the host adapter, including the missing-`pid` diagnostic and successful click response. |
| `/tmp/agentcompute-desktop-corrected-smoke/` | Corrected image boot: X11, active Driver user service, 234 apps from `list_apps`, and cleanup of smoke-owned resources. |

The first guest did not acquire its network until `/etc/netplan/10-incus.yaml` was repaired with `renderer: networkd`. The current recipe embeds that renderer and enables `systemd-networkd`. The corrected image passed the dedicated boot smoke with fingerprint `9d0da557210766289d59823a540050c3e7f328af66380974cc1ebd08e877bd11`; the token, screenshot, timing, and restart measurements below remain attributed to the repaired first guest.

## Image and session contract

The image is an amd64 Ubuntu 24.04 Xorg VM with a 16 GiB virtual disk, GDM automatic login as `automation`, `gnome-text-editor`, AT-SPI, Cua Driver, and X0tigervnc. The build uses the Ubuntu snapshot `20260911T000000Z`.

Cua Driver is pinned to the full `cua-driver-rs-0.28.1-linux-x86_64.tar.gz` archive with SHA-256 `a068b6e477893b77ced74bceccf7db7483cf140e8d54150ce5849b6252b90bcf`. Version 0.28.1 is a GitHub prerelease accepted under the project-specific pin exception; it is not a stable GitHub release.

The graphical user service keeps one Driver daemon attached to the Xorg session:

```text
/usr/local/bin/cua-driver serve --socket /run/user/1000/cua-driver.sock
```

The host starts a separate one-shot CLI process for each native operation. It preserves native tool names and JSON arguments rather than adding typed wrappers for individual Driver tools. The effective execution identity is UID 1000, working directory `/home/automation`, with this command shape:

```sh
HOME=/home/automation XDG_RUNTIME_DIR=/run/user/1000 \
  /usr/local/bin/cua-driver call \
  --socket /run/user/1000/cua-driver.sock \
  list_apps '{}'
```

Passing the empty JSON object positionally is required for these Incus exec calls; waiting for JSON on stdin leaves the exec websocket open. The retained status check found the daemon at the expected socket with PID 676 and standard permission mode. A nonexistent socket exited 1 and reported that the daemon was not running.

## Native Driver results

The probe launched `gnome-text-editor` as PID 1220 with window ID 31457284 and bounds 822 × 642. Its first `get_window_state` response had snapshot ID `s00000001`, 13 accessibility elements, and one `tab panel = "New Document"` entry. Element token `s00000001:5` identified the **New tab** button.

Calling `click` with only that token printed `Missing required integer field: pid` even though the pinned `click` input schema has no required fields. The CLI exited 0 and returned plain text. The host therefore cannot equate a zero exit status with success: it accepts structured JSON or native `[OK]` text and otherwise fails closed.

Calling `click` again with PID 1220 and the token returned structured JSON:

```json
{
  "delivery": {"mode": "background"},
  "effect": "unverifiable",
  "route": "accessibility"
}
```

That response records delivery, not the GUI effect. A separate `get_window_state` call returned snapshot ID `s00000002`, 19 elements, and two `tab panel = "New Document"` entries. The before and after PNGs also differ. The state and image snapshots, not `effect: "unverifiable"`, prove that the tab count changed from one to two across calls.

Representative single-operation timings from the repaired guest:

| Operation | Total | Driver exec | PNG pull |
| --- | ---: | ---: | ---: |
| Initial `list_apps` | 361 ms | 358 ms | — |
| `launch_app` | 777 ms | 777 ms | — |
| Before-click `get_window_state` with tree | 580 ms | 511 ms | 67 ms |
| Successful token `click` with PID | 308 ms | 307 ms | — |
| After-click `get_window_state` with tree | 744 ms | 685 ms | 58 ms |
| `get_desktop_state` | 208 ms | 161 ms | 45 ms |
| Native 640-pixel window capture | 281 ms | 207 ms | 73 ms |
| Post-restart `list_apps` | 347 ms | 343 ms | — |

A four-call comparison on the maximized 1214 × 768 editor window showed the cost and response-size difference from omitting the accessibility tree:

| `get_window_state` case | Driver exec | PNG pull | stdout | PNG |
| --- | ---: | ---: | ---: | ---: |
| Tree, default dimension | 282 ms | 59 ms | 11,665 B | 30,366 B |
| No tree, default dimension | 180 ms | 55 ms | 318 B | 30,366 B |
| Tree, `max_dimension=1280` | 216 ms | 67 ms | 11,665 B | 30,366 B |
| No tree, `max_dimension=1280` | 195 ms | 59 ms | 318 B | 30,366 B |

These are individual calls, not throughput benchmarks.

## Screenshot behavior

Native `get_desktop_state` has no `max_dimension` field in the pinned Linux schema. Its direct capture returned a 1280 × 800 PNG and matching screen metadata. The host `desktop.screenshot` convenience applies a requested desktop bound after pulling the PNG.

Native `get_window_state` does accept `max_dimension`. With `max_dimension=640`, the Driver returned a 640 × 500 PNG while preserving the original `window_bounds` width of 822. The host response uses that original coordinate width to report an image-to-Driver scale of `822 / 640 = 1.284375`, so pixel coordinates from the bounded image map back to the native window coordinate space.

Screenshot bytes use the internal binary guest-file path, not the text exec path. Each call supplies a unique `--screenshot-out-file`, streams the PNG through the binary file API, removes the guest file, strips any embedded image payload from preserved JSON, and returns only a bearer URL plus dimensions and scale. URLs are served with `Cache-Control: no-store` and expire after at most five minutes or when the sandbox expires. Native structured JSON remains otherwise intact.

## VNC and restart recovery

The final recipe runs X0tigervnc on guest TCP port 5900 with no baked reusable credential. Access is limited by the private sandbox network or an explicitly created forward.

The direct spike reached the management-network VNC endpoint at `10.10.40.65:5900`. It read the RFB banner `RFB 003.008\n` before restart and again after restart. Restart-to-Driver-ready took 12.117 seconds, after which `list_apps` returned structured JSON in 347 ms and X0tigervnc was running under a new PID. This proves recovery for the repaired first guest only.

## Build measurements and release status

Both local builds used fresh work and output directories. Download time includes the Go toolchain, vendored distrobuilder source, Ubuntu base, snapshot CA package, and full Cua Driver archive. Compile time excludes downloads and assembly. Scratch usage was sampled every 100 ms, so an interval peak can be missed.

| Build evidence | Download | Compile | Assemble | Peak RSS | Scratch high-water | Metadata | Disk |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| First image | 5.849 s | 31.670 s | 842.017 s | 507,180 KiB | 7,625,043,968 B | 656 B | 744,611,840 B |
| Corrected rebuild | 5.824 s | 31.834 s | 392.512 s | 517,712 KiB | 7,621,808,128 B | 640 B | 745,013,248 B |

The first image artifacts were `incus.tar.xz` SHA-256 `3174b0a6e76d6e1b3e601a7ffc761589615205fed0124bd1f300ed52aa612f54` and `disk.qcow2` SHA-256 `ed3cfd45045f459b5b17ac5782a2e8f17f10e06b5805719ccbdf27a04bd22f37`. The corrected artifacts were `incus.tar.xz` SHA-256 `d7e5e20009425f2d2164770797e84e5d99611d0fe0d5a0b0e719032aa5c9cd9d` and `disk.qcow2` SHA-256 `42c8bdd02f8991822ecdb2093d02b760f781ab21e301a15ed1466830c2d36c2c`. Both qcow2 files report a 17,179,869,184-byte virtual size.

Protected bootstrap PR #23 and private bake run 34854323245 completed successfully. The desktop-aware publisher awaits deployment before the four-image bake. Desktop publication and catalog promotion remain pending; no desktop GHCR digest is claimed.

The real MCP acceptance lane passed capability registration and reached sandbox creation. OVN rejected the create because `/var/lib/ovn/ovnnb_db.db` could not write: `ovncentral01` had filled its 20 GiB root filesystem, with approximately 19 GiB in logs. This is a Phase 5 infrastructure defect, not a desktop result. The companion fleet work preserves evidence before the explicitly approved log truncation and addresses recurrence.
