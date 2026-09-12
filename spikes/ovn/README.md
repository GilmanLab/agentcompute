# OVN mechanism spike

**2026-09-12 — NOT SMOOTH.** Step 6 did not fail cleanly with central down:
network creation timed out and left an `Errored` network. Step 7 recreated
cross-member connectivity, but gateway and internet egress failed without
manual repair. The first pass's dataplane worked. This verdict uses steps
5–7 only; the earlier cleanup problem is a separate sequencing constraint.

- [Fleet implementation PR #14](https://github.com/GilmanLab/fleet/pull/14)
- [Address reservation PR #30](https://github.com/GilmanLab/root/pull/30)
- [Report and probe PR #15](https://github.com/GilmanLab/agentcompute/pull/15)
- [Northbound-unset reproduction](https://github.com/lxc/incus/issues/3948#issuecomment-5642832127)

The owner decides whether to use the documented bridge alternative. This
experiment does not change the design draft or choose another architecture.

## Retained state and Phase 5 handoff

The resume instruction supersedes the original rollback requirement. These
resources remain deliberately enabled despite the negative verdict:

| Resource | Retained configuration |
| --- | --- |
| `sandbox01` central | Ubuntu 26.04, `ovn-central` 26.03.0-2; running and enabled at boot |
| Northbound | `network.ovn.northbound_connection=tcp:10.10.40.10:6641`, consistent on all four members |
| Chassis southbound | `database=tcp:10.10.40.10:6642`, enabled on all four members |
| Geneve tunnels | VLAN 30: nas01 `.14`, lab01 `.11`, lab02 `.12`, lab03 `.13` |
| OVN uplink | Default-project `fast40-uplink`, type `physical`, `parent=fast40` on all four members |
| Uplink allocation | `ipv4.ovn.ranges=10.10.40.64-10.10.40.79` |
| Uplink gateway/DNS | `ipv4.gateway=10.10.40.1/24`, `dns.nameservers=10.10.40.1` |
| Restricted forward authorization | `ipv4.routes=10.10.40.64/28`; project subnet allowance scoped to that same block |
| Image-build network | Independent default-project `image-build` NAT bridge; allocated `10.57.139.1/24` |

**Chassis must remain enabled while the northbound key is set. Phase 5 must
repoint NB before any chassis change, and replace temporary central before
retiring it. Do not unset NB or disable chassis as spike cleanup.** The
plain-TCP central is the explicitly permitted temporary arrangement, not the
Phase 5 TLS design. No separate Incus server southbound key or interconnection
setting was introduced.

All disposable OVN projects, networks, forwards, guests, and copied project
image associations were removed. The image-build test guests were removed;
the shared `router` image and alias were preserved. Final NB topology was
empty; SB still listed all four chassis. No switch, gateway firewall, or
`networking` repository changes were needed.

## Image-build migration and corrected uplink instruction

**The macvlan-uplink instruction in `03b` was incorrect: macvlan networks do
not carry `ipv4.ovn.*` keys.** Applying its three proposed settings through
fleet produced this Incus error before any network update:

```text
Invalid option for network "fast40-macvlan" option "dns.nameservers"
```

The [physical network reference](https://linuxcontainers.org/incus/docs/main/reference/network_physical/)
documents the OVN keys. A direct profile attachment to a physical network
would not preserve macvlan behavior: Incus derives a physical NIC and passes
the host interface into the guest. See the
[NIC reference](https://linuxcontainers.org/incus/docs/main/reference/devices_nic/#available-nic-types).
That fallback was stopped before replacing the network or profile.

The owner instead authorized this fleet-managed sequence:

1. Define `image-build`, type `bridge`, on all four members with empty
   per-member configuration, then activate it with `ipv4.address=auto`,
   `ipv4.nat=true`, `ipv4.dhcp=true`, and `ipv6.address=none`.
2. Authorize the replacement network before changing the profile; move
   `image-build/default`'s `eth0` to it; then revoke access to the old network.
   Applying the final access restriction first was rejected because the old
   profile still referenced `fast40-macvlan`. The deploy now handles that
   ordering. `restricted.devices.nic=managed`, 2 CPUs, 2 GiB memory, and the
   10 GiB root disk on `data` remain unchanged.
3. Verify build egress and the existing image boot test before deleting the
   unused macvlan network and creating physical `fast40-uplink`.

A fleet-created `ovn-build-egress` guest on `lab02` received
`10.57.139.212/24`, gateway `10.57.139.1`, MTU 1500. This passed:

```sh
incus exec nas01:ovn-build-egress --project image-build -- \
  wget -T 20 -qO /dev/null https://dl-cdn.alpinelinux.org
```

The unchanged `spikes/images/smoke.sh` passed all six router-tool checks in
4.555 s with central running, and again in 4.062 s with central stopped.
Both runs exited 0 after cleanup and preserved the shared image/alias.
HTTPS egress also passed while central was stopped. The boot test uses
`incus exec`, not inbound guest reachability; no VLAN 40 inbound dependency
was found in that test or its image-publish caller. Image builds therefore
use neither the physical uplink nor OVN central.

The test used an export of the existing router image, fingerprint
`6e3183fe052e00d89c806c414cb3ff92f757884894414ea1cf83ecbb9ae20c7f`.
An initial automation invocation left stdin open, causing `incus init` to
wait for stdin configuration. It was stopped; no instance had been created.
The unchanged test passed with closed stdin. This was a runner error, not
an image-network failure.

## Environment and evidence

Incus servers: **7.4**. IncusOS: **202608242359**. Operator client: **7.3**.
Central: `10.10.40.10/24` on `sandbox01/enp2s0`, underlay MTU 1500.
The operator's route to the forward address used **`utun4`**, MTU 1280,
via the existing Tailscale subnet route.

Evidence files:

- [preflight.json](preflight.json): original baseline commands and observations.
- [execution.jsonl](execution.jsonl): fleet applications, connectivity probes,
  captures, outage/reboot observations, cleanup, and final dry-runs.
- [topology.jsonl](topology.jsonl): individual Incus commands, output, exit
  status, and duration for creation, failed attempts, and teardown.

Commands and output are recorded, including failures. Unrelated workstation
process listings and public trust-certificate material are explicitly redacted.
Recorder timestamps are UTC; gateway packet timestamps use the gateway's
local timezone. A client timeout is not reported as a clean server error.

Before chassis enablement, TCP SYN/SYN-ACK checks reached both central ports
from guests placed on each member; gateway capture showed the four member
management addresses as sources. The initial dependency gap was resolved by
[pyinfra-incus #41](https://github.com/meigma/pyinfra-incus/issues/41) and
[PR #42](https://github.com/meigma/pyinfra-incus/pull/42), released as 0.2.5.
Fleet pins that release rather than carrying a local OVN API writer.

## Step 5: first complete dataplane pass

The disposable `ac-ovn-spike` project used the Phase 2 restricted shape with
`features.networks=true`, managed NICs/disks, blocked privileged devices,
`restricted.networks.uplinks=fast40-uplink`, and
`restricted.networks.subnets=fast40-uplink:10.10.40.64/28`.

The subnet allowance initially failed because the uplink had no matching
`ipv4.routes` authorization. Fleet added only the reserved `/28`; it did
not add a gateway route or broaden the address allocation. Guest preparation
also exposed that the router image's base BusyBox lacks `httpd`; the helper
now installs `curl` and `busybox-extras` inside the disposable guests.
That incomplete preparation attempt was destroyed before the measured pass.

| Item | Observation |
| --- | --- |
| Network | `default`, OVN, `10.253.73.1/24`, NAT and DHCP enabled |
| Guest one | `lab01`, `10.253.73.2` |
| Guest two | `lab03`, `10.253.73.3` |
| Cross-member ping | 3/3 replies each direction; no packet loss |
| HTTPS egress | `curl -4 -fsS https://example.com` passed from both guests |
| Forward | `10.10.40.79:8080` → `10.253.73.2:8080` |
| Workstation forward | Exact body `ovn-spike-ok`, over the Tailscale route |
| SNAT | Both guests appeared as `10.10.40.64` at `sandbox01/enp2s0` |
| Network creation | 0.797 s from network POST through response |
| First cross-member ping | 3.626 s after network creation returned, including image copy, launches, and address discovery |
| Full topology preparation | 8.389 s, including guest packages, HTTP fixtures, and forward |
| Full connectivity/transfer probe | 9.371 s |

Representative VLAN 40 capture:

```text
02:53:52.259423 IP 10.10.40.64 > 10.10.40.10: ICMP echo request
02:53:53.543530 IP 10.10.40.64 > 10.10.40.10: ICMP echo request
```

Incus chose **1442** for both guest interfaces and `bridge.mtu`, 58 bytes
below the 1500-byte underlay. Each transfer was exactly **100,000,000 bytes**
(decimal 100 MB), guest-to-guest rather than through Tailscale, and matched
SHA-256 `a993f8c574e0fea8c1cdcbcd9408d9e2e107ee6e4d120edcfa11decd53fa0cae`.

| Direction | Central running | Central stopped |
| --- | --- | --- |
| lab03 → lab01 | 0.118847 s; 841,417,957 bytes/s | 0.100184 s; 998,163,379 bytes/s |
| lab01 → lab03 | 0.116090 s; 861,400,637 bytes/s | 0.147786 s; 676,654,080 bytes/s |

These are curl's transfer timings for one fixture in each direction, not a
capacity benchmark or sustained-throughput guarantee.

## Step 6: central outage and recovery

Fleet stopped central; the listener inspection showed no NB/SB listeners.
The complete probe still passed: cross-member ping, both HTTPS requests,
workstation forward, and both 100 MB transfers. Its wall time was 8.862 s.
Image-build's independent bridge also passed the boot/egress checks above.

A single new-network POST for `recovery` did **not** fail cleanly. The helper
terminated the client at its 60 s deadline with no server error text:

```json
{"created": false, "code": 124, "timeout_seconds": 60, "seconds": 60.014}
```

An `Errored` network remained and held external address `10.10.40.65`.
Restarting central did not remove it or make an identical fresh create
possible: the helper refused to adopt the existing name. After explicitly
deleting that failed disposable network, a fresh POST succeeded in 0.395 s.
This deletion is recovery work, not evidence of clean automatic recovery.
The second outage attempt reproduced a 60.009 s timeout and an `Errored`
network both before and after central restart. Deleting it allowed another
fresh POST in 0.291 s. No retry loop hides these failures.

## Step 7: teardown and repeat

The first complete topology was destroyed in **4.620 s**, including guests,
forward, networks, copied project images, and project. Recreating the same
project/network and members required no host or chassis mutation. The second
network POST succeeded in **0.587 s**; DHCP assigned `.2` and `.3` again,
and cross-member ping passed with MTU 1442.

However, preparation failed after **10.272 s** at `apk add`: the Alpine index
could not be fetched. Subsequent direct checks from both guests showed
failed gateway ping, DNS, and HTTPS. A later gateway ping had **4 transmitted,
0 received**. This was not treated as a package-name problem or hidden by
retries. The second pass did not reach its forward or large-transfer checks.

Read-only diagnostics found:

- NB contained the new logical router; SB bound the guests to lab01/lab03
  and its gateway port to lab01.
- External `.64` was reused with a new router MAC. The gateway neighbor was
  `FAILED`; sandbox01 still had the previous MAC as `STALE`.
- The overlapping gateway capture saw no `.64` ICMP/ARP traffic from the
  attempted guest pings. It did capture ordinary central/gateway ARP traffic.

These observations do not establish the root cause. No ARP flush, OVS or
chassis restart, uplink recreation, or alternate network architecture was
used to repair the repeat. The second central-outage creation test was still
executed as described above. Final disposable teardown succeeded in
**4.133 s**. Thus this is a failed repeat, not two passing lifecycle runs.

## External-address consumption

Measured for the successful sandbox:

- One NATed OVN network: **one external IPv4 address**, `.64`.
- One distinct forward listen address: **one additional IPv4 address**, `.79`.
- The two guests consume internal addresses, not two more VLAN 40 addresses.
- The auxiliary network consumed `.65`, including while its state was
  `Errored`; failed resources must be included in capacity accounting.

**Phase 5 planning number: two external IPv4 addresses for one sandbox with
one OVN network and one typical forward listen address.** More ports can
share that listen address; distinct listen addresses cost more addresses.
That sharing statement follows the forward API, rather than an additional
multi-port test in this spike.

The reserved 16-address range therefore holds at most eight such sandboxes
before operational headroom. A hypothetical 64-address external allocation
would hold at most 32 on the same arithmetic, not a tested capacity and not
a claim that all addresses of an arbitrary `/26` are available. The owner
must account for existing allocations, subnet boundaries, failed resources,
and headroom. No `/26` expansion was made.

## Reboot and service-update findings

Before the resume, lab03 was explicitly rebooted through the upstream
receipt-backed fleet operation while central was unreachable. Management TCP
returned approximately **116.7 s** after the request; a new boot ID was
confirmed approximately **125 s** after it. All four members returned ONLINE.
IncusOS reported:

```text
Failed to start initrd-cleanup-root.service - Cleanup of the root partition.
Startup finished in 44.592s (firmware) + 4.878s (loader) + 658ms (kernel) + 2.672s (initrd) + 44.072s (userspace) = 1min 36.874s
```

The OVN controller started with central unavailable; no OVN-specific startup
failure was observed. Available pre-OVN boot records from nas01, lab01, and
lab02 did not contain that initrd failure. Their startup totals were
2min20.287s, 1min30.568s, and 1min33.097s. These are comparison records, not a
controlled reboot of a never-enabled member. They do **not** establish that
OVN caused the initrd failure. No additional reboot was performed during the
resume, and a central-reachable reboot was not separately tested.

IncusOS service PUT replaces the wrapped service configuration and can
persist the desired document even when runtime application fails. It has no
network-style confirmation rollback. Upstream 0.2.5 supplies that operation
and preserves omitted TLS material; stored equality alone is not a runtime
health assertion. See the
[service-contract finding](https://github.com/meigma/pyinfra-incus/issues/41#issuecomment-5641891596).

The earlier NB-unset attempt produced:

```text
failed to notify peer 10.10.10.11:8443: failed to connect to unix:/run/ovn/ovnnb_db.sock: failed to open connection: dial unix /run/ovn/ovnnb_db.sock: connect: no such file or directory
```

The matching [Incus issue #3948](https://github.com/lxc/incus/issues/3948)
was already closed; the exact local reproduction was added in the linked
comment rather than a duplicate issue. No fixed release was verified here.
After the failed unset, NB values disagreed across member caches. A supported
targeted set on lab01 refreshed the peers. Fleet now checks every member,
never unsets NB, and never disables chassis. This explains the Phase 5
sequencing rule, but is not the reason for the NOT SMOOTH verdict.

## Rerunning the probe

Use the implementation worktrees/PRs above. In `fleet/cluster`, the ordering
for the original macvlan installation is image-build migration and boot/egress
verification first, then the OVN deploy. Both expose pyinfra dry-runs:

```sh
FLEET_IMAGE_BUILD_CERT_FILE=/path/to/existing-public-ci.crt \
  uv run --locked pyinfra inventory.py src/fleet_cluster/deploys/image_build.py -v --dry
uv run --locked pyinfra inventory.py src/fleet_cluster/deploys/ovn.py -v --dry
```

Replace `--dry` with `--yes` only when applying the intended configuration.
Central control is a separate fleet deploy, from that same directory:

```sh
FLEET_OVN_CENTRAL_STATE=stopped uv run --locked pyinfra sandbox01 \
  --user josh --sudo src/fleet_cluster/deploys/ovn_central.py -v --yes
FLEET_OVN_CENTRAL_STATE=running uv run --locked pyinfra sandbox01 \
  --user josh --sudo src/fleet_cluster/deploys/ovn_central.py -v --yes
```

In `agentcompute`, with authenticated `incus`, Python 3, Bash, jq, and curl:

```sh
python3 spikes/ovn/topology.py --log /tmp/ovn-commands.jsonl create </dev/null
bash spikes/ovn/probe.sh nas01 ac-ovn-spike default one two \
  http://10.10.40.79:8080/index.html ovn-spike-ok </dev/null
python3 spikes/ovn/topology.py --log /tmp/ovn-commands.jsonl create-network </dev/null
python3 spikes/ovn/topology.py --log /tmp/ovn-commands.jsonl delete-network </dev/null
python3 spikes/ovn/topology.py --log /tmp/ovn-commands.jsonl destroy </dev/null
```

`create` refuses an existing project and preserves failed setup for diagnosis.
`destroy` checks its ownership tag and deletes only the disposable project's
resources, including copied image associations. `create-network` makes one
bounded attempt; its client deadline does not cancel or roll back server
work. Inspect the network after any timeout. The helper never modifies
central, chassis, the default-project uplink, or the image-build profile.

`probe.sh` checks an existing OVN topology and different-member placement,
reads MTU, pings both directions, checks HTTPS and the exact forward body,
and transfers/hash-checks the 100 MB fixture in both directions. It writes
the downloaded fixture inside each disposable guest but creates no Incus
resources. Both guests need curl and the HTTP fixture prepared by the helper.

## Final verification

- Fleet OVN dry-run: all ten operations unchanged, 3.500 s.
- Fleet image-build dry-run: all four operations unchanged, 2.191 s;
  the daemon-assigned bridge subnet was preserved rather than reapplying `auto`.
- Existing host-network dry-run: all four members unchanged, 2.286 s.
- Central running/enabled; all four SB chassis present; no disposable NB topology.
- Only `default` and `image-build` projects remain; no image-build test instances.
- Ruff checks passed; mypy passed 16 source files; fleet pytest **39 passed**.
- Companion address-plan site: `moon run docs:build` passed.

The retained configuration is converged. OVN lifecycle repeatability is not
qualified by this spike.

## 2026-09-12 — recreate and gateway diagnosis

**Verdict: FALLBACK**, under the owner's follow-up rule. A fresh nas01-gateway
cycle passed; a lab01-gateway cycle failed before and after one authorized
reboot. No fallback architecture was implemented. The failed keeper was removed
through fleet; the uplink, chassis configuration, central, and unrelated
instances were otherwise left in place as requested.

### One-command cycle

From this repository, with the existing Incus remote and SSH access to
`sandbox01` and gw01:

```sh
bash spikes/ovn/probe.sh cycle --evidence-dir "$(mktemp -d)"
```

This creates, probes, and destroys the owned disposable topology. It records
commands, elapsed times, network/router identity, NB/SB state, gateway neighbor
state, OVS journals, Incus debug monitors, and a bounded ARP capture. Monitor
readiness requires the client's successful `/1.0/events?all-projects=true`
WebSocket connection, not a sleep or a running PID.

The probe checks both directions of cross-member ping and 100 MB HTTP transfers
with SHA-256, both guests' gateway ping, DNS and HTTPS, and the workstation's
exact forward body. Probe-tool packages are downloaded on the workstation and
installed with signature verification through Incus file transfer: broken guest
egress must not prevent the remaining datapath checks from running.

There is one create attempt and no automatic recovery. The cycle never stops
central, creates a keeper, changes chassis configuration, or clears neighbors.
`--keep-on-failure` explicitly preserves failed resources for diagnosis;
otherwise teardown runs even after a failed probe.

### Cycle results

[Structured cycle table](diagnosis-2026-09-12/cycle-table.json). External
addresses below are in `10.10.40.0/24`; router names are NB logical routers.
Gateway placement comes from SB binding, not the container's member.

| Run | NB router | Gateway | External IP | Router MAC | Result | Seconds |
| --- | --- | --- | --- | --- | --- | ---: |
| clean-1 | incus-net24-lr | lab01 | .64 | 10:66:6a:49:f5:82 | Fail; bootstrap blocked by egress | 50.489 |
| keeper-1 | incus-net26-lr | nas01 | .65 | 10:66:6a:ed:13:7f | Pass | 29.756 |
| keeper-2 | incus-net27-lr | lab03 | .65 | 10:66:6a:6c:ad:18 | Pass | 32.190 |
| keeper-3 | incus-net28-lr | lab01 | .65 | 10:66:6a:14:21:7e | Fail; north-south only | 61.306 |
| outage-fixture | incus-net29-lr | lab01 | .65 | 10:66:6a:ab:ba:f8 | Fail; north-south only | 55.432 |
| post-outage | incus-net31-lr | nas01 | .65 | 10:66:6a:ea:40:3b | Pass | 31.078 |
| gateway-pre-1 | incus-net32-lr | lab02 | .64 | 10:66:6a:33:3f:c7 | Pass | 33.092 |
| gateway-pre-2 | incus-net33-lr | lab02 | .64 | 10:66:6a:74:95:5e | Pass | 29.904 |
| gateway-pre-3 | incus-net34-lr | lab02 | .64 | 10:66:6a:1f:21:eb | Pass | 31.142 |
| gateway-pre-4 | incus-net35-lr | lab02 | .64 | 10:66:6a:97:88:42 | Pass | 32.274 |
| gateway-pre-5 | incus-net36-lr | nas01 | .64 | 10:66:6a:ef:03:ee | Pass | 32.470 |
| gateway-pre-6 | incus-net37-lr | lab01 | .64 | 10:66:6a:b2:7a:65 | Fail; north-south only | 60.817 |
| gateway-post-1 | incus-net38-lr | lab01 | .64 | 10:66:6a:43:84:fe | Fail; north-south only | 61.572 |

All full probes after `clean-1` passed east-west ping and both 100 MB hash
checks, including the failed lab01-gateway cycles. Those failures comprised
gateway ping, DNS and HTTPS from both guests, plus the workstation forward.
The first cycle used guest-side package bootstrap; fallback checks established
cross-member ping but no 100 MB or forward fixture was installed.

Times include capture/setup/probe/cleanup work, except the explicitly retained
`clean-1` and `outage-fixture`. The former was deleted after the neighbor-clear
test; the latter after the outage experiment. All other cycles completed their
own teardown. No disposable topology remains.

### Hypotheses and bounded recovery

- **H3, stale gw01 neighbor state:** clearing only `.64` on gw01 did not restore
  gateway ping in the unchanged failed clean topology. With the keeper holding
  `.64`, clearing only the failed router's `.65` entry also did not restore
  connectivity. Both bounded sandbox01 ARP captures remained empty. A stale
  gw01 entry is not a sufficient explanation for these failures.
- **H1, last-consumer uplink teardown:** the keeper kept an OVN consumer alive,
  but its three cycles were pass/pass/fail. Failure followed lab01 gateway
  placement even without last-consumer teardown. H1 was not established as a
  teardown regression; the keeper did not qualify and was removed.
- **H2, reboot-recoverable chassis corruption:** the requested recovery criterion
  failed. Lab01 still failed as gateway after reboot. Do not interpret the
  successful nas01 control as lab01 recovery.

The original OVS journal, before sustained rate limiting, reports:

```text
system@ovs-system: failed to add fast40 as port: Device or resource busy
could not add network device fast40 to ofproto (Device or resource busy)
```

Incus lists `fast40-uplink` and `soak01` as consumers of lab01's `fast40`.
`soak01` has an active raw macvlan NIC on that parent. Its configuration was
unchanged, and the NIC was up before and after reboot. The same OVS errors
recurred in the fresh-boot cycle. **Inference:** the active macvlan is the likely
competing attachment; IncusOS's supported API does not expose kernel RX-handler
ownership or an `ovs-vsctl show` equivalent. We did not remove that NIC to test
the inference and do not claim a confirmed kernel/OVS corruption cause.

Evidence: [parent consumer](diagnosis-2026-09-12/lab01-parent-consumer.json),
[original journal](diagnosis-2026-09-12/lab01-original-ovs-journal.json),
[post-reboot errors](diagnosis-2026-09-12/lab01-post-reboot-ovs-conflict.json).
Persistent gateway failure reported as
[lxc/incus#3986](https://github.com/lxc/incus/issues/3986), including the
macvlan precondition and the distinction between unsupported parent sharing and
silently accepting/selecting an unprogrammable gateway.

### Reboot evidence

Fleet submitted exactly one lab01 reboot, request
`phase3-recreate-lab01-20260912`. ONLINE status and a different boot ID were both
required before the post-reboot cycle.

| Observation | Before | After |
| --- | --- | --- |
| Boot ID | f9f71dd5a051466283d7da6fb860c528 | b5e39bb2a73f421c9b89a935f9f116b2 |
| Incus | 7.4 | 7.4 |
| IncusOS | 202608242359 | 202609100026 |
| Kernel | 7.1.10-zabbly+ | 7.2.4-zabbly+ |
| soak01 macvlan eth1 | Up | Up |
| lab01 gateway probe | Fail | Fail |

Lab01 returned ONLINE after **146.549 s**. The reboot activated an OS/kernel
update, so this was not a same-software reboot comparison. The failure persisted
across that change. See [reboot comparison](diagnosis-2026-09-12/reboot-comparison.json)
and [readiness observation](diagnosis-2026-09-12/reboot-watch/reboot.json).

### Northbound outage

The single follow-up central outage reproduced the creation problem: the client
deadline expired after **60.019 s**, leaving an `Errored` network reserving `.66`.
Restarting central did not change that state. Explicit deletion succeeded;
without a service, chassis, uplink, or neighbor repair, the subsequent fresh
cycle passed on nas01 in **31.078 s**.

This outage fixture already had broken north-south traffic before central was
stopped; it is not new evidence of egress surviving an outage. The original
spike's outage observations remain separate.

Reported as [lxc/incus#3985](https://github.com/lxc/incus/issues/3985).
See [outage results](diagnosis-2026-09-12/outage/outage.json) and its command and
monitor logs. The hypothesis that connection initialization escapes the
transaction timeout remains source-based, not a stack-trace diagnosis.

### Address accounting and final state

A tested sandbox consumes **two external addresses**: one NAT router address
and one forward listen address. Multiple ports on the same forward address do
not require additional addresses. The 16-address allocation has capacity for
eight such sandboxes with no keeper, or seven plus one spare while a keeper
reserves one address. The keeper was removed and now consumes zero.

After the final failed cycle:

- All four cluster members ONLINE; central's four units active; four SB chassis.
- No disposable project, keeper, NB logical topology, or Errored network remains.
- Fleet OVN dry-run: all ten operations unchanged, **3.429 s**.
- Fleet keeper dry-run: absent and unchanged, **0.696 s**.
- Uplink/chassis/central retained as the owner requested; `soak01` unchanged
  apart from its authorized host reboot and automatic restart.

The [execution log](diagnosis-2026-09-12/execution.jsonl) records fleet
operations and final checks. Per-cycle directories contain the command,
probe, monitor, ARP, and NB/SB evidence. Published OVS journal responses retain
timestamp, host, boot ID when available, and message; unrelated journal
metadata is projected out and marked in command records. Original raw captures
were preserved privately before this reduction.

Publication checks: fleet pytest **39 passed**; mypy passed **17 source files**;
Ruff passed for the new fleet deploy and both Python probe helpers; Bash syntax,
Python compilation, and the actual cycle CLI help passed. The companion
address-plan site built successfully with `moon run docs:build`. No additional
live cycle was run after the owner's FALLBACK condition was established.
