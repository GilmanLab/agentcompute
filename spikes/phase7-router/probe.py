#!/usr/bin/env python3
"""Disposable restricted-project spike for router helpers and netem."""

from __future__ import annotations

import json
import re
import subprocess
import time
from pathlib import Path

REMOTE = "nas01:"
PROJECT = "ac-p7-rtr-impair"
IMAGE_PROJECT = "image-build"
IMAGE_FP = "1664a567b720a52bd890439bbf18f2c3809cae5c9461c5dd5032b1d9cf012426"
UPLINK = "fast40-uplink"
LAN = "lan"
WAN = "wan"
LAN_GW = "192.168.50.1/24"
WAN_GW = "10.99.0.1/24"
MEMBER = "lab01"
HELPERS = Path(__file__).resolve().parents[2] / "images" / "router"
EVIDENCE_DIR = Path(__file__).resolve().parent
EVENTS: list[dict] = []
RESULT: dict = {"project": PROJECT, "status": "running"}


def incus(*args: str, check: bool = True, timeout: int = 180, quiet: bool = False) -> subprocess.CompletedProcess[str]:
    started = time.monotonic()
    result = subprocess.run(["incus", *args], capture_output=True, text=True, timeout=timeout)
    event = {
        "args": args,
        "code": result.returncode,
        "seconds": round(time.monotonic() - started, 3),
        "stdout": result.stdout.strip() if not quiet else "",
        "stderr": result.stderr.strip(),
    }
    if not quiet or result.returncode:
        EVENTS.append(event)
        if not quiet:
            print(json.dumps(event), flush=True)
    if check and result.returncode:
        raise RuntimeError(result.stderr.strip() or result.stdout.strip() or f"exit {result.returncode}")
    return result


def query(path: str, method: str = "GET", data: dict | None = None, check: bool = True, timeout: int = 180, quiet: bool = False):
    args = ["query", REMOTE + path, "--request", method, "--wait"]
    if data is not None:
        args.extend(["--data", json.dumps(data)])
    result = incus(*args, check=check, timeout=timeout, quiet=quiet)
    text = result.stdout.strip()
    return json.loads(text) if text else None


def guest(name: str, command: str, check: bool = True, timeout: int = 120) -> subprocess.CompletedProcess[str]:
    return incus(
        "exec",
        REMOTE + name,
        "--project",
        PROJECT,
        "--",
        "bash",
        "-c",
        command,
        check=check,
        timeout=timeout,
    )


def guest_bg(name: str, command: str) -> subprocess.Popen[str]:
    started = time.monotonic()
    proc = subprocess.Popen(
        ["incus", "exec", REMOTE + name, "--project", PROJECT, "--", "bash", "-c", command],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    EVENTS.append({"args": ("exec-bg", name, command), "pid": proc.pid, "seconds": round(time.monotonic() - started, 3)})
    return proc


def finish_bg(proc: subprocess.Popen[str], timeout: int = 15) -> tuple[str, str, int | None]:
    try:
        stdout, stderr = proc.communicate(timeout=timeout)
        return stdout.strip(), stderr.strip(), proc.returncode
    except subprocess.TimeoutExpired:
        proc.kill()
        stdout, stderr = proc.communicate()
        return stdout.strip(), stderr.strip(), None


def wait_exec(name: str, seconds: int = 90) -> None:
    deadline = time.time() + seconds
    while time.time() < deadline:
        result = guest(name, "/bin/true", check=False)
        if result.returncode == 0:
            return
        time.sleep(2)
    raise TimeoutError(f"{name} agent not ready")


def wait_ipv4(name: str, iface: str, seconds: int = 90) -> str:
    deadline = time.time() + seconds
    while time.time() < deadline:
        state = query(f"/1.0/instances/{name}/state?project={PROJECT}", quiet=True)
        network = (state or {}).get("network") or {}
        for address in network.get(iface, {}).get("addresses") or []:
            if address.get("family") == "inet" and address.get("scope") == "global":
                return address["address"]
        time.sleep(2)
    raise TimeoutError(f"{name} {iface} has no global IPv4")


def ping_avg_ms(output: str) -> float:
    match = re.search(r"min/avg/max(?:/[a-z]+)? = [\d.]+/([\d.]+)/", output)
    if not match:
        match = re.search(r"rtt min/avg/max/[^\s]+ = [\d.]+/([\d.]+)/", output)
    if not match:
        raise RuntimeError(f"no ping average in: {output}")
    return float(match.group(1))


def create_instance(name: str, nics: dict[str, str]) -> None:
    devices = {
        "root": {"type": "disk", "path": "/", "pool": "data", "size": "2GiB"},
    }
    for iface, network in nics.items():
        devices[iface] = {"type": "nic", "network": network, "name": iface}
    query(
        f"/1.0/instances?project={PROJECT}&target={MEMBER}",
        "POST",
        {
            "name": name,
            "type": "container",
            "profiles": [],
            "source": {"type": "image", "fingerprint": IMAGE_FP},
            "config": {
                "limits.cpu": "1",
                "limits.memory": "256MiB",
                "security.privileged": "false",
            },
            "devices": devices,
        },
    )
    incus("start", REMOTE + name, "--project", PROJECT)


def destroy() -> None:
    instances = query(f"/1.0/instances?project={PROJECT}&recursion=1", check=False) or []
    if isinstance(instances, list):
        for inst in instances:
            name = inst.get("name") if isinstance(inst, dict) else None
            if name:
                incus("delete", "-f", REMOTE + name, "--project", PROJECT, check=False)
    networks = query(f"/1.0/networks?project={PROJECT}&recursion=1", check=False) or []
    if isinstance(networks, list):
        for network in networks:
            name = network.get("name") if isinstance(network, dict) else str(network).rsplit("/", 1)[-1]
            if name:
                query(f"/1.0/networks/{name}?project={PROJECT}", "DELETE", check=False)
    images = query(f"/1.0/images?project={PROJECT}&recursion=1", check=False) or []
    if isinstance(images, list):
        for image in images:
            fingerprint = image.get("fingerprint") if isinstance(image, dict) else None
            if fingerprint:
                incus("image", "delete", REMOTE + fingerprint, "--project", PROJECT, check=False)
    incus("project", "delete", REMOTE + PROJECT, check=False)


def push_helpers(name: str) -> None:
    for helper in ("nat", "route", "dhcp", "wg"):
        incus(
            "file",
            "push",
            str(HELPERS / helper),
            f"{REMOTE}{name}/opt/router/{helper}",
            "--project",
            PROJECT,
            "--mode",
            "0755",
        )


def run() -> None:
    existing = query("/1.0/projects?recursion=1")
    names = {project["name"] for project in existing}
    if PROJECT in names:
        raise RuntimeError(f"refusing to adopt existing project {PROJECT}")

    query(
        "/1.0/projects",
        "POST",
        {
            "name": PROJECT,
            "config": {
                "features.images": "true",
                "features.profiles": "true",
                "features.networks": "true",
                "restricted": "true",
                "restricted.containers.nesting": "block",
                "restricted.containers.privilege": "unprivileged",
                "restricted.containers.lowlevel": "block",
                "restricted.cluster.target": "allow",
                "restricted.snapshots": "allow",
                "restricted.devices.nic": "managed",
                "restricted.devices.disk": "managed",
                "restricted.networks.uplinks": UPLINK,
                "user.p7.router.spike": "true",
            },
        },
    )
    RESULT["project_created"] = True

    query(
        f"/1.0/networks?project={PROJECT}",
        "POST",
        {
            "name": LAN,
            "type": "ovn",
            "config": {
                "network": "none",
                "ipv4.address": LAN_GW,
                "ipv4.nat": "false",
                "ipv4.dhcp": "true",
                "ipv6.address": "none",
            },
        },
    )
    query(
        f"/1.0/networks?project={PROJECT}",
        "POST",
        {
            "name": WAN,
            "type": "ovn",
            "config": {
                "network": UPLINK,
                "ipv4.address": WAN_GW,
                "ipv4.nat": "true",
                "ipv4.dhcp": "true",
                "ipv6.address": "none",
                "dns.nameservers": "10.10.40.1",
            },
        },
    )
    RESULT["networks"] = {
        "lan": query(f"/1.0/networks/{LAN}?project={PROJECT}"),
        "wan": query(f"/1.0/networks/{WAN}?project={PROJECT}"),
    }

    incus(
        "image",
        "copy",
        REMOTE + IMAGE_FP,
        REMOTE,
        "--project",
        IMAGE_PROJECT,
        "--target-project",
        PROJECT,
    )
    create_instance("rtr", {"eth0": LAN, "eth1": WAN})
    create_instance("client", {"eth0": LAN})
    create_instance("wanprobe", {"eth0": WAN})

    for name in ("rtr", "client", "wanprobe"):
        wait_exec(name)
    rtr_lan = wait_ipv4("rtr", "eth0")
    client_ip = wait_ipv4("client", "eth0")
    wanprobe_ip = wait_ipv4("wanprobe", "eth0")
    RESULT["eth1_before_nat"] = query(f"/1.0/instances/rtr/state?project={PROJECT}").get("network", {}).get("eth1", {})

    rtr_cfg = query(f"/1.0/instances/rtr?project={PROJECT}")
    RESULT["rtr_privileged"] = (rtr_cfg or {}).get("config", {}).get("security.privileged", "")

    push_helpers("rtr")
    for helper in ("nat", "route", "dhcp", "wg"):
        help_out = guest("rtr", f"/opt/router/{helper} --help")
        RESULT.setdefault("help", {})[helper] = help_out.stdout.strip() or help_out.stderr.strip()

    caps = guest("rtr", "grep -E 'Cap(Prm|Eff):' /proc/1/status; id -u")
    RESULT["rtr_caps"] = caps.stdout.strip()
    RESULT["nft_before"] = guest("rtr", "nft list ruleset").stdout.strip()

    first = guest("rtr", "/opt/router/nat --mode port-restricted --inside eth0 --outside eth1")
    rtr_wan = wait_ipv4("rtr", "eth1")
    second = guest("rtr", "/opt/router/nat --mode port-restricted --inside eth0 --outside eth1")
    nft_after = guest("rtr", "nft list tables; nft list table ip router_nat")
    RESULT["addresses"] = {
        "rtr_lan": rtr_lan,
        "rtr_wan": rtr_wan,
        "client": client_ip,
        "wanprobe": wanprobe_ip,
    }
    RESULT["nat"] = {
        "first": {"code": first.returncode, "stderr": first.stderr.strip()},
        "second": {"code": second.returncode, "stderr": second.stderr.strip()},
        "nft": nft_after.stdout.strip(),
        "idempotent": first.returncode == 0 and second.returncode == 0,
    }

    RESULT["client_routes_before"] = guest("client", "ip route show").stdout.strip()
    RESULT["client_route_command"] = f"ip route replace default via {rtr_lan}"
    RESULT["wan_route_command"] = f"ip route replace 10.99.0.0/24 via {rtr_lan}"
    guest("client", RESULT["client_route_command"])
    RESULT["client_routes_after"] = guest("client", "ip route show").stdout.strip()
    guest("rtr", "/opt/router/route --to 198.51.100.0/24 --via 10.99.0.1 --dev eth1")
    guest("rtr", "/opt/router/route --to 198.51.100.0/24 --via 10.99.0.1 --dev eth1")
    RESULT["rtr_routes"] = guest("rtr", "ip route show").stdout.strip()

    baseline = guest("client", f"ping -c 5 -W 2 {wanprobe_ip}", timeout=30)
    icmp_cap = guest_bg("wanprobe", "tcpdump -n -l -i eth0 -c 6 icmp")
    time.sleep(1)
    ping_while = guest("client", f"ping -c 3 -W 2 {wanprobe_ip}", check=False, timeout=20)
    icmp_out, icmp_err, icmp_code = finish_bg(icmp_cap)
    RESULT["ping_baseline"] = {
        "code": baseline.returncode,
        "stdout": baseline.stdout.strip(),
        "avg_ms": ping_avg_ms(baseline.stdout) if baseline.returncode == 0 else None,
    }
    RESULT["translated_source"] = {
        "tcpdump": icmp_out,
        "tcpdump_err": icmp_err,
        "tcpdump_code": icmp_code,
        "ping": ping_while.stdout.strip(),
        "expected_src": rtr_wan,
        "saw_rtr_wan": rtr_wan in icmp_out,
        "saw_client": client_ip in icmp_out,
    }

    udp_cap = guest_bg("wanprobe", "tcpdump -n -l -i eth0 -c 4 udp port 9999")
    time.sleep(1)
    guest("client", f"echo p7nat >/dev/udp/{wanprobe_ip}/9999", check=False)
    udp_out, udp_err, udp_code = finish_bg(udp_cap)
    ct = guest("rtr", "cat /proc/net/nf_conntrack 2>/dev/null | grep -F udp | grep -F 9999 || true", check=False)
    RESULT["port_restricted"] = {
        "udp_capture": udp_out,
        "udp_err": udp_err,
        "udp_code": udp_code,
        "conntrack": ct.stdout.strip(),
        "capture_src_is_rtr": rtr_wan in udp_out,
        "capture_src_is_client": client_ip in udp_out,
    }

    client_cap = guest_bg("client", "tcpdump -n -l -i eth0 -c 2 udp port 4000")
    time.sleep(1)
    guest("wanprobe", f"echo unsolicited >/dev/udp/{rtr_wan}/4000", check=False)
    uns_out, uns_err, uns_code = finish_bg(client_cap, timeout=8)
    RESULT["unsolicited_inbound"] = {
        "tcpdump": uns_out,
        "err": uns_err,
        "code": uns_code,
        "forwarded": bool(uns_out.strip()),
    }

    netem = guest("rtr", "tc qdisc replace dev eth1 root handle 1: netem delay 100ms loss 5%")
    qdisc = guest("rtr", "tc qdisc show dev eth1")
    delayed = guest("client", f"ping -c 5 -W 2 {wanprobe_ip}", timeout=30, check=False)
    guest("rtr", "tc qdisc del dev eth1 root")
    cleared_qdisc = guest("rtr", "tc qdisc show dev eth1")
    cleared = guest("client", f"ping -c 5 -W 2 {wanprobe_ip}", timeout=30, check=False)
    RESULT["netem"] = {
        "apply_code": netem.returncode,
        "qdisc": qdisc.stdout.strip(),
        "delayed": delayed.stdout.strip(),
        "delayed_avg_ms": ping_avg_ms(delayed.stdout) if delayed.returncode == 0 else None,
        "cleared_qdisc": cleared_qdisc.stdout.strip(),
        "cleared": cleared.stdout.strip(),
        "cleared_avg_ms": ping_avg_ms(cleared.stdout) if cleared.returncode == 0 else None,
        "impair_apply": "net.impair(sandbox=..., instance='rtr', nic='eth1', latency_ms=100, loss_percent=5)",
        "impair_clear": "net.impair(sandbox=..., instance='rtr', nic='eth1', clear=True)",
    }

    dhcp1 = guest(
        "rtr",
        "/opt/router/dhcp --interface eth0 --range 192.168.50.150,192.168.50.160 --gateway 192.168.50.2",
        check=False,
    )
    dhcp2 = guest(
        "rtr",
        "/opt/router/dhcp --interface eth0 --range 192.168.50.150,192.168.50.160 --gateway 192.168.50.2",
        check=False,
    )
    dhcp_ps = guest("rtr", "cat /run/router-dhcp.eth0.pid; ps | grep -F dnsmasq | grep -v grep || true", check=False)
    if dhcp1.returncode == 0:
        pid = guest("rtr", "cat /run/router-dhcp.eth0.pid", check=False).stdout.strip()
        if pid:
            guest("rtr", f"kill {pid} || true", check=False)
    RESULT["dhcp"] = {
        "first": {"code": dhcp1.returncode, "stderr": dhcp1.stderr.strip(), "stdout": dhcp1.stdout.strip()},
        "second": {"code": dhcp2.returncode, "stderr": dhcp2.stderr.strip()},
        "ps": dhcp_ps.stdout.strip(),
    }

    guest("rtr", "wg genkey > /run/wg0.key && wg pubkey < /run/wg0.key > /run/wg0.pub", check=False)
    pubkey = guest("rtr", "cat /run/wg0.pub 2>/dev/null || true", check=False).stdout.strip()
    wg = guest(
        "rtr",
        f"/opt/router/wg --interface wg0 --address 10.8.0.1/24 --private-key-file /run/wg0.key "
        f"--peer {pubkey or 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='} "
        f"--endpoint {wanprobe_ip}:51820 --allowed-ips 10.8.0.0/24",
        check=False,
    )
    RESULT["wg"] = {
        "code": wg.returncode,
        "stderr": wg.stderr.strip(),
        "stdout": wg.stdout.strip(),
        "iface": guest("rtr", "ip link show wg0 2>/dev/null || true", check=False).stdout.strip(),
    }

    masquerade = guest("rtr", "/opt/router/nat --mode masquerade --inside eth0 --outside eth1")
    RESULT["masquerade"] = {
        "code": masquerade.returncode,
        "stderr": masquerade.stderr.strip(),
        "nft": guest("rtr", "nft list table ip router_nat").stdout.strip(),
    }
    guest("rtr", "/opt/router/nat --mode port-restricted --inside eth0 --outside eth1")
    RESULT["status"] = "ok"


def main() -> None:
    EVIDENCE_DIR.mkdir(parents=True, exist_ok=True)
    try:
        run()
    except Exception as exc:
        RESULT["status"] = "error"
        RESULT["error"] = str(exc)
        raise
    finally:
        try:
            destroy()
            RESULT["cleaned"] = True
        except Exception as exc:
            RESULT["cleaned"] = False
            RESULT["cleanup_error"] = str(exc)
        remaining = query("/1.0/projects?recursion=1", check=False) or []
        RESULT["project_gone"] = PROJECT not in {p.get("name") for p in remaining if isinstance(p, dict)}
        (EVIDENCE_DIR / "events.jsonl").write_text("".join(json.dumps(e) + "\n" for e in EVENTS))
        (EVIDENCE_DIR / "evidence.json").write_text(json.dumps(RESULT, indent=2) + "\n")
        print(json.dumps({"status": RESULT.get("status"), "project_gone": RESULT.get("project_gone")}))


if __name__ == "__main__":
    main()
