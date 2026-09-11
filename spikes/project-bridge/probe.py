#!/usr/bin/env python3
"""Probe member-only bridge activation; stop before guests if activation fails."""
import json
import subprocess

REMOTE = "nas01:"
OWNED_PROJECTS = []
OWNED_NETWORKS = []


def incus(*args, check=True):
    result = subprocess.run(["incus", *args], capture_output=True, text=True, timeout=180)
    print(json.dumps({"args": args, "code": result.returncode, "stdout": result.stdout.strip(), "stderr": result.stderr.strip()}), flush=True)
    if check and result.returncode:
        raise RuntimeError(result.stderr.strip())
    return result


def query(path, method="GET", data=None):
    args = ["query", REMOTE + path, "--request", method]
    if data is not None:
        args += ["--data", json.dumps(data)]
    return incus(*args)


def run():
    projects = json.loads(query("/1.0/projects?recursion=1").stdout)
    networks = json.loads(query("/1.0/networks?project=default&recursion=1").stdout)
    assert not {"ac-a", "ac-b"} & {p["name"] for p in projects}, "pre-existing project; refusing adoption"
    assert not {"ac-a-default", "ac-b-default"} & {n["name"] for n in networks}, "pre-existing network; refusing adoption"
    try:
        for name in ["a", "b"]:
            network = f"ac-{name}-default"
            config = {
                "features.images": "true", "features.profiles": "true", "features.networks": "false",
                "restricted": "true", "restricted.containers.nesting": "block",
                "restricted.containers.privilege": "unprivileged", "restricted.containers.lowlevel": "block",
                "restricted.devices.nic": "managed", "restricted.devices.disk": "managed",
                "restricted.devices.gpu": "block", "restricted.devices.pci": "block",
                "restricted.devices.proxy": "block", "restricted.devices.usb": "block",
                "restricted.devices.unix-block": "block", "restricted.devices.unix-char": "block",
                "restricted.devices.unix-hotplug": "block", "restricted.devices.infiniband": "block",
                "restricted.networks.access": network,
                "user.agentcompute.version": "1", "user.agentcompute.host": "lab01",
            }
            query("/1.0/projects", "POST", {"name": f"ac-{name}", "config": config})
            OWNED_PROJECTS.append(f"ac-{name}")
            query("/1.0/networks?project=default&target=lab01", "POST", {"name": network, "type": "bridge", "config": {}})
            OWNED_NETWORKS.append(network)
            query("/1.0/networks?project=default", "POST", {"name": network, "type": "bridge", "config": {"ipv4.address": "auto", "ipv4.nat": "true", "ipv4.dhcp": "true", "ipv6.address": "none", "user.agentcompute.sandbox": name}})
    finally:
        failures = []
        for network in reversed(OWNED_NETWORKS):
            result = incus("network", "delete", REMOTE + network, "--project", "default", check=False)
            if result.returncode:
                failures.append(network)
        for project in reversed(OWNED_PROJECTS):
            result = incus("project", "delete", REMOTE + project, check=False)
            if result.returncode:
                failures.append(project)
        projects = json.loads(incus("project", "list", REMOTE, "--format=json").stdout)
        networks = json.loads(incus("network", "list", REMOTE, "--project", "default", "--format=json").stdout)
        residue = [p["name"] for p in projects if p["name"] in OWNED_PROJECTS] + [n["name"] for n in networks if n["name"] in OWNED_NETWORKS]
        print(json.dumps({"cleanup_failures": failures, "residue": residue}), flush=True)
        assert not failures and not residue


if __name__ == "__main__":
    run()
