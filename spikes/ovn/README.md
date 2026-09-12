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
