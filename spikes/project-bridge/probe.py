#!/usr/bin/env python3
"""Probe restricted projects, all-member opaque bridges, and member-local L2."""
import datetime
import json
from pathlib import Path
import secrets
import subprocess
import time

REMOTE = "nas01:"
HOST = "lab01"
PROJECTS = []
NETWORKS = []
EVENTS = []
RESULT = {"status": "running", "host": HOST}


def incus(*args, check=True):
    started = time.monotonic()
    result = subprocess.run(["incus", *args], capture_output=True, text=True, timeout=180)
    event = {"args": args, "code": result.returncode, "seconds": round(time.monotonic() - started, 3)}
    if result.returncode or args[0] == "exec":
        event.update(stdout=result.stdout.strip(), stderr=result.stderr.strip())
    EVENTS.append(event)
    print(json.dumps(event), flush=True)
    if check and result.returncode:
        raise RuntimeError(result.stderr.strip())
    return result


def query(path, method="GET", data=None, check=True):
    args = ["query", REMOTE + path, "--request", method, "--wait"]
    if data is not None:
        args += ["--data", json.dumps(data)]
    response = incus(*args, check=check)
    return json.loads(response.stdout) if response.stdout.strip() else None


def guest(project, name, command, check=True):
    return incus("exec", REMOTE + name, "--project", project, "--", "sh", "-c", command, check=check)


def launch(project, name, network, host, fingerprint):
    incus("image", "copy", REMOTE + fingerprint, REMOTE, "--project", "image-build", "--target-project", project)
    query(f"/1.0/instances?project={project}&target={host}", "POST", {
        "name": name, "type": "container", "profiles": [],
        "source": {"type": "image", "fingerprint": fingerprint},
        "config": {"limits.cpu": "1", "limits.memory": "512MiB"},
        "devices": {"root": {"type": "disk", "path": "/", "pool": "data", "size": "2GiB"},
                    "eth0": {"type": "nic", "network": network, "name": "eth0"}},
    })
    incus("start", REMOTE + name, "--project", project)


def run():
    members = [m["server_name"] for m in query("/1.0/cluster/members?recursion=1")]
    RESULT["members"] = members
    assert HOST in members and len(members) == 4
    projects = query("/1.0/projects?recursion=1")
    assert not {"ac-a", "ac-b"} & {p["name"] for p in projects}, "refusing existing projects"
    fingerprint = query("/1.0/images/aliases/router?project=image-build")["target"]
    other = next(m for m in members if m != HOST)
    try:
        for name in ["a", "b"]:
            while True:
                network = "ac" + secrets.token_hex(4)
                existing = query("/1.0/networks?project=default&recursion=1")
                if network not in {n["name"] for n in existing}:
                    break
            stamp = datetime.datetime.now(datetime.timezone.utc)
            config = {
                "features.images": "true", "features.profiles": "true", "features.networks": "false",
                "restricted": "true", "restricted.containers.nesting": "block",
                "restricted.containers.privilege": "unprivileged", "restricted.containers.lowlevel": "block",
                "restricted.cluster.target": "allow",
                "restricted.devices.nic": "managed", "restricted.devices.disk": "managed",
                "restricted.devices.gpu": "block", "restricted.devices.pci": "block",
                "restricted.devices.proxy": "block", "restricted.devices.usb": "block",
                "restricted.devices.unix-block": "block", "restricted.devices.unix-char": "block",
                "restricted.devices.unix-hotplug": "block", "restricted.devices.infiniband": "block",
                "restricted.networks.access": network,
                "user.agentcompute.version": "1", "user.agentcompute.host": HOST,
                "user.agentcompute.created_at": stamp.isoformat(),
                "user.agentcompute.expires_at": (stamp + datetime.timedelta(minutes=15)).isoformat(),
                "user.agentcompute.subject": "phase2-spike",
            }
            project = f"ac-{name}"
            query("/1.0/projects", "POST", {"name": project, "config": config})
            PROJECTS.append(project)
            started = time.monotonic()
            for member in members:
                query(f"/1.0/networks?project=default&target={member}", "POST", {"name": network, "type": "bridge", "config": {}})
                if network not in NETWORKS:
                    NETWORKS.append(network)
            query("/1.0/networks?project=default", "POST", {"name": network, "type": "bridge", "config": {
                "ipv4.address": "auto", "ipv4.nat": "true", "ipv4.dhcp": "true", "ipv6.address": "none",
                "user.agentcompute.sandbox": name, "user.agentcompute.name": "default", "user.agentcompute.version": "1"}})
            RESULT.setdefault("bridges", []).append({"sandbox": name, "physical": network, "activation_seconds": round(time.monotonic() - started, 3)})
            launch(project, "router", network, HOST, fingerprint)
            for attempt in range(15):
                egress = guest(project, "router", "ping -c 1 -W 2 10.10.40.1", check=False)
                if egress.returncode == 0:
                    break
                time.sleep(1)
            assert egress.returncode == 0, "NAT egress failed"
        forbidden = incus("config", "device", "add", "nas01:router", "wrong", "nic", "network=" + NETWORKS[1], "name=eth1", "--project", "ac-a", check=False)
        assert forbidden.returncode != 0 and "not allowed" in forbidden.stderr.lower(), forbidden.stderr
        RESULT["cross_sandbox_refusal"] = forbidden.stderr.strip()
        launch("ac-a", "other", NETWORKS[0], other, fingerprint)
        # Static distinct endpoints avoid two independent DHCP servers choosing the same IP.
        guest("ac-a", "router", "ip addr add 192.0.2.10/24 dev eth0")
        guest("ac-a", "other", "ip addr add 192.0.2.11/24 dev eth0")
        separated = guest("ac-a", "other", "ping -c 2 -W 2 192.0.2.10", check=False)
        assert separated.returncode != 0, "unexpected cross-member L2 connectivity"
        RESULT["cross_member"] = {"first": HOST, "second": other, "ping_exit": separated.returncode, "output": separated.stdout.strip()}
        RESULT["status"] = "passed"
    finally:
        failures = []
        for project in reversed(PROJECTS):
            for instance in query(f"/1.0/instances?project={project}&recursion=1"):
                if incus("delete", "-f", REMOTE + instance["name"], "--project", project, check=False).returncode:
                    failures.append(instance["name"])
            for image in query(f"/1.0/images?project={project}&recursion=1"):
                if incus("image", "delete", REMOTE + image["fingerprint"], "--project", project, check=False).returncode:
                    failures.append(image["fingerprint"])
        for network in reversed(NETWORKS):
            if incus("network", "delete", REMOTE + network, "--project", "default", check=False).returncode:
                failures.append(network)
        for project in reversed(PROJECTS):
            if incus("project", "delete", REMOTE + project, check=False).returncode:
                failures.append(project)
        residue = {}
        for member in members:
            projects = query(f"/1.0/projects?recursion=1&target={member}")
            networks = query(f"/1.0/networks?project=default&recursion=1&target={member}")
            residue[member] = [p["name"] for p in projects if p["name"] in PROJECTS] + [n["name"] for n in networks if n["name"] in NETWORKS]
        RESULT.update(cleanup_failures=failures, residue=residue, events=EVENTS)
        results_path = Path(__file__).resolve().parents[1] / "results.json"
        previous = json.loads(results_path.read_text())
        previous["second_run"] = RESULT
        results_path.write_text(json.dumps(previous, indent=2) + "\n")
        print(json.dumps({"status": RESULT["status"], "cleanup_failures": failures, "residue": residue}), flush=True)
        assert not failures and not any(residue.values())


if __name__ == "__main__":
    run()
