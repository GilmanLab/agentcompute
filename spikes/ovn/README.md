# OVN mechanism spike — paused at the upstream dependency

Status on 2026-09-11: **BLOCKED; live experiment not started.**

The operator chose **“File issue and pause spike”** after preflight found that
pyinfra-incus 0.2.4, the latest published release, has no IncusOS OVN service
operation or explicit member reboot operation. The missing chassis writer is a
step-2 prerequisite, found before installing central in step 1.

Upstream request: [meigma/pyinfra-incus#41](https://github.com/meigma/pyinfra-incus/issues/41).
No fleet-local replacement API writer or unreviewed dependency pin was added.

This is not a negative OVN dataplane result. Neither **SMOOTH** nor
**NOT SMOOTH** has been established by the experiment. The operator-approved
pause replaces execution until the dependency is available; the documented
bridge fallback remains the owner's decision.

## Observed baseline

- All four cluster members are ONLINE. Incus server is 7.4, IncusOS release
  `202608242359`; the operator's Incus client is 7.3.
- All four `/os/1.0/services/ovn` documents have `enabled=false` and empty
  `database`, `tunnel_address`, and `tunnel_protocol` values.
- Live host network documents contain `fast30` with the member's storage
  address and address-less `fast40`, role `instances`.
- Existing managed networks are `incusbr0` and `fast40-macvlan`; projects are
  `default` and `image-build`. They were not modified.
- `sandbox01` is reachable through its existing Tailscale SSH path as `josh`.
  It reports Ubuntu 26.04, `10.10.40.10/24` on `enp2s0`, interface MTU 1500,
  and Tailscale MTU 1280. These are host values, not OVN MTU findings.
- `ovn-central`, `ovn-common`, and `openvswitch-common` are absent on
  `sandbox01`. An `apt-get --simulate install --no-install-recommends
  ovn-central` proposed only those three packages, with central candidate
  `26.03.0-2`; no installation was performed.
- Fleet's existing network deploy dry-run reports no change for every member.
  The recorded final preflight dry-run took 2.317 seconds, exit 0. This is a
  host-network convergence check, not an OVN-deploy dry-run.

[preflight.json](preflight.json) contains the recorded baseline commands,
stdout, stderr, exit codes, and elapsed times, including the complete network
list and four service documents. The package-query exit 1 records package
absence, not a failed installation.

The meta-repository's primary checkout was stale. Current address-plan content
was read from `GilmanLab/root` master, and the design was read from the existing
`feat/agentcompute-design` worktree without editing it. Fleet commits
`2ef3177`, `d0e0b5e`, and `ff9d4b0` confirm LACP, VLAN 40 admission, and
IncusOS-owned `fast40`. The stale journal T48 entries were not used as current
network facts.

## Mutation and cleanup record

No cluster or sandbox mutation was executed. No central installation, chassis
configuration, server setting, physical uplink, project, instance, network,
forward, reboot, firewall rule, or address-plan amendment was created.
Consequently, no cluster rollback or remote cleanup was necessary. Fleet has
no implementation changes or PR from this paused attempt. The design and
networking repository remain unchanged.

The proposed spike allocation is **`10.10.40.64–10.10.40.79`**. It is outside
DHCP `.200–.250` and sandbox01 `.10`, but **has not been reserved or activated**.
A fresh address-conflict check and explicit reservation remain prerequisites
before use. The live Incus allocation listing contained no use of that block;
that does not prove absence of independently assigned hosts.

## Service update safety finding

Source inspection at IncusOS commit
`6060b55d1a1a7dabf235b56ec2bface71ec196df` found a contract different from
the system-network deploy:

- PUT replaces the complete service configuration, wrapped as
  `{"config": {...}}`; a bare configuration object can decode as zero config.
- `OVN.Update` assigns the new configuration and defers saving it before
  starting/reconfiguring services. An apply error can persist the desired
  document even though runtime configuration failed.
- The service has no network-style confirmation timeout or `:confirm`
  endpoint. Its `:reset` action restarts services; it is not a host reboot.

The [upstream safety comment](https://github.com/meigma/pyinfra-incus/issues/41#issuecomment-5641891596)
links the exact handlers and schema. These are source findings, not observed
live failures or proof of the installed release's error recovery. The
upstream operation must not treat stored-document equality as runtime health
after a failed apply.

## Resume constraints

1. Consume upstream OVN service support through fleet, with behavioral tests
   and a dry-run before applying. The service document is separate from the
   full system-network document; do not rewrite host networking to enable it.
2. Confirm the service-specific update/error contract. Do not assume the
   system-network `confirmation_timeout` and `:confirm` protocol applies to
   services. A stored desired configuration alone is not service health.
3. Run central temporarily on sandbox01's VLAN 40 address. Prove reachability
   from every member before enabling chassis, then use each member's VLAN 30
   address for Geneve.
4. Incus's ordinary server setting is
   `network.ovn.northbound_connection=tcp:<central>:6641`. Southbound belongs
   in the chassis service's `database=tcp:<central>:6642`. Do not invent an
   ordinary `network.ovn.southbound_connection` setting or enable
   interconnection for this single-cluster spike.
5. Define the physical uplink in fleet with `parent=fast40` on every member.
   Account for forward listen-address authorization: the forward reference
   requires an uplink `ipv4.routes` allowance or project subnet allowance;
   do not assume `ipv4.ovn.ranges` alone grants forward addresses.
6. Reuse the Phase 2 restricted-project shape from
   [project-bridge/probe.py](../project-bridge/probe.py), changing network
   isolation to `features.networks=true` and scoping uplink access. Place
   guests explicitly on different members.
7. Execute the originally requested central-outage test and two complete
   topology lifecycles, then the central-unreachable reboot test and cleanup.
   A service configuration may be retained only after reboot safety passes.

Sources:

- [IncusOS OVN service](https://linuxcontainers.org/incus-os/docs/main/reference/services/ovn/)
- [OVN service schema](https://github.com/lxc/incus-os/blob/main/incus-osd/api/service_ovn.go)
- [Incus OVN setup](https://linuxcontainers.org/incus/docs/main/howto/network_ovn_setup/)
- [Network forwards](https://linuxcontainers.org/incus/docs/main/howto/network_forwards/)
- [Project configuration](https://linuxcontainers.org/incus/docs/main/reference/projects/)
- [Current address plan](https://github.com/GilmanLab/root/blob/master/docs/docs/reference/networking/address-plan.md)

## Outstanding acceptance evidence

| Evidence | Status |
| --- | --- |
| Cross-member ping, internet curl, workstation forward | Not run |
| Source NAT observed on VLAN 40 | Not run |
| OVN instance MTU and large-transfer behavior | Not measured |
| Central-down traffic and clean network-create failure | Not run |
| Recovery after central restart | Not run |
| Second pass after complete teardown; timings | Not run |
| Node reboot with central unreachable | Not run |
| Final OVN fleet deploy no-diff | Unavailable; no OVN deploy exists yet |
| Final existing fleet host-network no-diff | Passed; four members unchanged |
| External addresses per sandbox | Not measured |

Phase 5's capacity number is still unknown. The documentation describes one
router external address per NATed OVN network and one address per distinct
forward listen address, with multiple port mappings able to share a listen
address. **Planning hypothesis, not measured evidence:** one network plus one
shared forward address uses two external IPv4 addresses; additional distinct
listen addresses increase consumption. Do not size or approve a /26 from this
paused run.

## Reusable probe

`probe.sh` operates on an already-created topology. It does not install
packages or create, start, stop, or delete Incus resources. Its help describes
the required arguments and guest dependencies. Setup, NAT-source capture,
large-transfer measurement, central control, reboot, and teardown remain
separate operations under the original fleet/disposable-project boundaries.

The connectivity path has **not** been qualified against an OVN topology in
this paused attempt. Baseline inspection is not a substitute for steps 5–8.

Validation performed:

- `bash -n spikes/ovn/probe.sh`: exit 0.
- `bash spikes/ovn/probe.sh --help`: exit 0.
- Probe against the existing `default/incusbr0`: exit 1,
  `probe: network incusbr0 type is not ovn`, in 0.384 seconds. It made only
  network list/show reads and stopped before instance execution or HTTP.
  This verifies refusal of a bridge, not OVN connectivity.

Exact commands and output are included in `preflight.json`. No project-wide
test suite or live topology test was run for this paused experiment.
