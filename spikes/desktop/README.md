# Desktop Phase 6 spike

**2026-09-14 — the corrected image passed X11/Driver boot qualification and the complete production-stdio MCP acceptance scenario.** One-shot Driver calls retain token continuity through the persistent guest daemon. Screenshots remain URL-only; the adapter neither retries input nor silently changes delivery mode.

## Evidence and scope

The results use these evidence sources:

| Evidence | What it establishes |
| --- | --- |
| `/tmp/agentcompute-desktop-phase6-image/metrics.json` | First local image build measurements and artifact hashes. |
| `/tmp/agentcompute-desktop-spike/` | Direct native Driver results from a guest booted from the first build after its netplan was repaired in place. Each operation has a report, stdout, stderr, and any PNG pulled from the guest. `retained-evidence.json` preserves the timing comparison, status checks, native 640-pixel capture, VNC checks, and restart timing. |
| `/tmp/agentcompute-desktop-phase6-final/metrics.json` | Corrected local rebuild measurements and artifact hashes. It is build evidence, not live Driver or MCP acceptance evidence. |
| `internal/desktop/testdata/` | Captured Driver catalog and result fixtures used by the host adapter, including the missing-`pid` diagnostic and successful click response. |
| `/tmp/agentcompute-desktop-corrected-smoke/` | Corrected image boot: X11, active Driver user service, 234 apps from `list_apps`, and cleanup of smoke-owned resources. |
| `/tmp/agentcompute-desktop-mcp-evidence/` | Before/after PNGs from the corrected image's real MCP token interaction. The full acceptance run passed in 158.45 s after OVN recovery. |
| `/tmp/agentcompute-desktop-mcp-final-evidence/` | Post-refactor repeat: fetched PNGs show a single document before the native token click and two document tabs afterward. Full acceptance passed in 160.32 s. |
| `/tmp/agentcompute-publisher-rollout/result.json` | Approved controller replacement, matching recovered identities, new publisher standby, and no-drift Terraform plan. |
| `/tmp/agentcompute-desktop-published-evidence/` | Protected bake 34872818589: four immutable releases, desktop build measurements, X11/Driver boot qualification, and verified fetch-back. |
| `/tmp/agentcompute-desktop-published-mcp-evidence/` | Exact catalog from PR #27, without GHCR credentials: full MCP acceptance passed in 182.26 s. Fetched PNGs show one document before the native token click and two document tabs afterward. |

The first guest did not acquire its network until `/etc/netplan/10-incus.yaml` was repaired with `renderer: networkd`. The current recipe embeds that renderer and enables `systemd-networkd`. The corrected image passed the dedicated boot smoke with fingerprint `9d0da557210766289d59823a540050c3e7f328af66380974cc1ebd08e877bd11`. Direct native measurements use the repaired first guest; the later MCP runs identify their image separately.

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

The corrected image also completed this interaction through `desktop.call` in one MCP `execute` program. A background attempt exposed an upstream delivery limitation: `click` returned `effect: "unverifiable"` with `route: "global_input"` but the next snapshot still showed one tab. The acceptance program therefore supplies the native `window_id` and explicitly requests `delivery_mode: "foreground"`. Its single click returned the same unverifiable effect, routed through accessibility; subsequent tree and PNG observations proved two tabs. No host-side retry or implicit foreground fallback was added.

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

The corrected-image MCP acceptance created a separate viewer on the default OVN network, created a TCP forward, restarted the guest, and observed `desktop.info.ready == true` again. The address reported by `desktop.info.vnc`, `10.10.40.67:5900`, answered with `RFB 003.008\n`. The representative client retained only its private LAN NIC. Its complete create/wait/screenshot/list-apps program took 23.624 s. After shortening sandbox lifetime, the running reaper made the original screenshot URL return 404 at 27.120 s after expiry.

The post-refactor repeat passed the same full scenario in 160.32 s. Its representative program took 22.764 s, and screenshot expiry returned 404 at 26.804 s after sandbox expiry. The reported VNC endpoint again answered at `10.10.40.67:5900`.

The published-image run used the complete catalog from PR #27 without GHCR
credentials and passed in 182.26 s. Its representative program took
26.822 s. One native foreground token click changed the editor from a
single document to two document tabs, confirmed by a fresh accessibility
tree and the fetched before/after PNGs. Reboot restored Driver readiness
and the reported VNC endpoint at `10.10.40.67:5900`; the running reaper
made the screenshot URL return 404 at 21.773 s after sandbox expiry.

## Build measurements and release status

Both local builds used fresh work and output directories. Download time includes the Go toolchain, vendored distrobuilder source, Ubuntu base, snapshot CA package, and full Cua Driver archive. Compile time excludes downloads and assembly. Scratch usage was sampled every 100 ms, so an interval peak can be missed.

| Build evidence | Download | Compile | Assemble | Peak RSS | Scratch high-water | Metadata | Disk |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| First image | 5.849 s | 31.670 s | 842.017 s | 507,180 KiB | 7,625,043,968 B | 656 B | 744,611,840 B |
| Corrected rebuild | 5.824 s | 31.834 s | 392.512 s | 517,712 KiB | 7,621,808,128 B | 640 B | 745,013,248 B |
| Protected published build | 6.624 s | 33.283 s | 403.364 s | 505,216 KiB | 7,620,071,424 B | 628 B | 742,923,264 B |

The first image artifacts were `incus.tar.xz` SHA-256 `3174b0a6e76d6e1b3e601a7ffc761589615205fed0124bd1f300ed52aa612f54` and `disk.qcow2` SHA-256 `ed3cfd45045f459b5b17ac5782a2e8f17f10e06b5805719ccbdf27a04bd22f37`. The corrected artifacts were `incus.tar.xz` SHA-256 `d7e5e20009425f2d2164770797e84e5d99611d0fe0d5a0b0e719032aa5c9cd9d` and `disk.qcow2` SHA-256 `42c8bdd02f8991822ecdb2093d02b760f781ab21e301a15ed1466830c2d36c2c`. Both qcow2 files report a 17,179,869,184-byte virtual size.

Protected bootstrap PR #23 and private bake run 34854323245 completed successfully. The approved publisher rollout replaced `ghrunner01` through its existing Terraform module, restored both escrowed identities, and qualified a standby with fingerprint `c6b3815e002e101d256980a1a80e55bfd8053ea3dea324383cd0f8fd43ddc60e`. Squid and scheduling recovered, and the post-apply plan reported no changes.

Image PR #25 merged as `e4333f245b4e81c8d7753038f0ddf04a620bd0a2`. [Protected bake 34872818589](https://github.com/GilmanLab/agentcompute-images/actions/runs/34872818589) built, boot-qualified, published, and fetched back all four images. The desktop pipeline took 674.806 s, including those stages. Its immutable reference is:

```text
ghcr.io/gilmanlab/agentcompute/ubuntu-24.04-desktop@sha256:5dc4e120a79dd06ad6784e69474f0617387f74cb98685af8844170b7165ea8e2
```

The published desktop artifacts are `incus.tar.xz` SHA-256 `c673c40c973731471f404bd59a8cfebfeb043bdd9a20c2076894b13e48fa75d6` and `disk.qcow2` SHA-256 `40abe7aea48ebe8bb989cbe71afc4b4376a0ecb5dd23ed2630ba73985de7ed0d`; the split-image Incus fingerprint is `cc9ed27aa5cde44aaf075cc78474b038011172c979549ae43cf538fc5ece9a93`. Qualification observed X11, the active automation-user Driver service, and 232 apps including `gnome-text-editor`, then removed its owned VM and image. [Catalog PR #27](https://github.com/GilmanLab/agentcompute/pull/27) contains the real digest and remains unmerged by request.

The first anonymous catalog-backed attempt could not import the new desktop
package because GitHub created it private. The owner made it public. The
unfixed server then downloaded the published image but rejected it with
`smoke check systemctl is-active incus-gh-runner-guest.path: exit 4`; the
reproduction failed in 68.06 s. Catalog qualification had treated every VM
as a GitHub runner. The reconciler now selects desktop qualification from
the catalog capability: X11, the active automation-user Driver unit, and
native `list_apps` executed as UID/GID 1000 with that user's session
environment. Router and runner checks are unchanged.

The original MCP blocker was a Phase 5 OVN outage: 19,307,134,976 bytes of logs filled central's 20 GiB root. The approved fleet recovery preserved complete signature counts and log boundaries before truncation, then recycled only the three Incus daemons retaining stale CA trust. No central database or northd process restarted. Logging limits are now active; details are in [fleet PR #20](https://github.com/GilmanLab/fleet/pull/20) and [the central OVN recovery runbook PR](https://github.com/GilmanLab/root/pull/34).

Live acceptance also corrected two adapter boundaries. Native private Incus images need an image-access secret in the create request; the image existed even though the unauthenticated pull reported it missing. VNC forward discovery must use the same DHCP-lease fallback as forward creation because guest NIC state can briefly lack addresses after reboot. The passing run exercised both corrections without publishing the temporary image or delaying Driver readiness for networking.
