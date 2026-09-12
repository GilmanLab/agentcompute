#!/usr/bin/env python3
"""Disposable OVN topology helper for the Phase 3 live spike.

Subcommands: create, destroy, create-network, delete-network.

Mutations are limited to project ac-ovn-spike. The default project, uplink,
central, and chassis are read-only. create does not destroy on failure.
destroy requires the ownership tag, deletes project images, and does not
treat HTTP 404/not-found as a failed GET. create-network is one bounded
attempt with no retries or hidden cleanup.
"""
import argparse
import json
import subprocess
import sys
import time
from pathlib import Path

REMOTE = "nas01"
PROJECT = "ac-ovn-spike"
NETWORK = "default"
AUX_NETWORK = "recovery"
INSTANCE_ONE = "one"
INSTANCE_TWO = "two"
MEMBER_ONE = "lab01"
MEMBER_TWO = "lab03"
SUBNET = "10.253.73.1/24"
AUX_SUBNET = "10.253.74.1/24"
IMAGE_PROJECT = "image-build"
IMAGE_ALIAS = "router"
POOL = "data"
ROOT_SIZE = "2GiB"

OVN_CENTRAL_ADDRESS = "10.10.40.10"
OVN_UPLINK = "fast40-uplink"
OVN_UPLINK_RANGE = "10.10.40.64-10.10.40.79"
OVN_UPLINK_SUBNET = "10.10.40.64/28"
FORWARD_LISTEN = "10.10.40.79"
FORWARD_PORT = 8080
DNS_NAMESERVERS = "10.10.40.1"

OWNER_KEY = "user.ovn-spike.owned"
OWNER_VALUE = "true"
MARKER = "ovn-spike-ok"
LARGE_BYTES = 100_000_000
LARGE_SHA256 = "a993f8c574e0fea8c1cdcbcd9408d9e2e107ee6e4d120edcfa11decd53fa0cae"
ALPINE_MAIN = "https://dl-cdn.alpinelinux.org/alpine/v3.22/main"

COMMAND_TIMEOUT = 180
IMAGE_TIMEOUT = 300
NETWORK_CREATE_TIMEOUT = 60
WAIT_IP_SECONDS = 90
WAIT_IP_POLL = 2

LOG = None


class TopologyError(Exception):
    """Command or topology contract failure; create does not clean up."""


def remote_prefix():
    return REMOTE if REMOTE.endswith(":") else REMOTE + ":"


def log_event(event):
    LOG.write(json.dumps(event, default=str) + "\n")
    LOG.flush()


def is_absent(result):
    if result.returncode == 0:
        return False
    text = f"{result.stderr}\n{result.stdout}".lower()
    if "timeout" in text:
        return False
    return "not found" in text or "doesn't exist" in text or "does not exist" in text


def incus(*args, check=True, timeout=COMMAND_TIMEOUT):
    started = time.monotonic()
    try:
        result = subprocess.run(
            ["incus", *args], capture_output=True, text=True, timeout=timeout
        )
        error = None
    except subprocess.TimeoutExpired as exc:
        stdout = exc.stdout if isinstance(exc.stdout, str) else (exc.stdout or b"").decode("utf-8", "replace")
        stderr = exc.stderr if isinstance(exc.stderr, str) else (exc.stderr or b"").decode("utf-8", "replace")
        event = {
            "args": args,
            "code": None,
            "seconds": round(time.monotonic() - started, 3),
            "stdout": stdout,
            "stderr": stderr,
            "error": "timeout",
        }
        log_event(event)
        if check:
            raise TopologyError("timeout: " + " ".join(args))
        return subprocess.CompletedProcess(["incus", *args], 124, stdout, stderr)
    event = {
        "args": args,
        "code": result.returncode,
        "seconds": round(time.monotonic() - started, 3),
        "stdout": result.stdout,
        "stderr": result.stderr,
        "error": None if result.returncode == 0 else (result.stderr.strip() or result.stdout.strip() or f"exit {result.returncode}"),
    }
    log_event(event)
    if check and result.returncode:
        raise TopologyError(event["error"])
    return result


def assert_mutation_scope(path, method, data=None):
    if method == "GET":
        return
    if path == "/1.0/projects" and method == "POST":
        name = (data or {}).get("name")
        if name != PROJECT:
            raise TopologyError(f"refusing project create name {name!r}")
        return
    if path.startswith("/1.0/projects/" + PROJECT):
        return
    if f"project={PROJECT}" in path:
        return
    raise TopologyError(f"refusing mutation outside {PROJECT}: {method} {path}")


def query(path, method="GET", data=None, check=True, timeout=COMMAND_TIMEOUT):
    assert_mutation_scope(path, method, data)
    args = ["query", remote_prefix() + path, "--request", method, "--wait"]
    if data is not None:
        args += ["--data", json.dumps(data)]
    response = incus(*args, check=check, timeout=timeout)
    if response.returncode != 0:
        return None
    return json.loads(response.stdout) if response.stdout.strip() else None


def get_existing(path):
    result = incus("query", remote_prefix() + path, "--request", "GET", "--wait", check=False)
    if result.returncode == 0:
        return json.loads(result.stdout) if result.stdout.strip() else {}
    if is_absent(result):
        return None
    raise TopologyError(f"GET {path} failed: {result.stderr.strip() or result.stdout.strip() or f'exit {result.returncode}'}")


def list_or_absent(path):
    result = incus("query", remote_prefix() + path, "--request", "GET", "--wait", check=False)
    if result.returncode == 0:
        return json.loads(result.stdout) if result.stdout.strip() else []
    if is_absent(result):
        return []
    raise TopologyError(f"GET {path} failed: {result.stderr.strip() or result.stdout.strip() or f'exit {result.returncode}'}")


def emit(payload, code=0):
    json.dump(payload, sys.stdout, indent=2)
    sys.stdout.write("\n")
    raise SystemExit(code)


def project_config():
    # Phase 2 restricted shape from spikes/project-bridge/probe.py, with
    # features.networks=true and the documented OVN uplink/subnet keys.
    # No extra restricted.* keys beyond that plus Phase 2 device blocks.
    return {
        "features.images": "true",
        "features.profiles": "true",
        "features.networks": "true",
        "restricted": "true",
        "restricted.containers.nesting": "block",
        "restricted.containers.privilege": "unprivileged",
        "restricted.containers.lowlevel": "block",
        "restricted.cluster.target": "allow",
        "restricted.devices.nic": "managed",
        "restricted.devices.disk": "managed",
        "restricted.devices.gpu": "block",
        "restricted.devices.pci": "block",
        "restricted.devices.proxy": "block",
        "restricted.devices.usb": "block",
        "restricted.devices.unix-block": "block",
        "restricted.devices.unix-char": "block",
        "restricted.devices.unix-hotplug": "block",
        "restricted.devices.infiniband": "block",
        "restricted.networks.uplinks": OVN_UPLINK,
        "restricted.networks.subnets": f"{OVN_UPLINK}:{OVN_UPLINK_SUBNET}",
        OWNER_KEY: OWNER_VALUE,
    }


def require_owned_project():
    projects = list_or_absent("/1.0/projects?recursion=1")
    found = next((p for p in projects if p.get("name") == PROJECT), None)
    if found is None:
        return None
    config = found.get("config") or {}
    if config.get(OWNER_KEY) != OWNER_VALUE:
        raise TopologyError(f"project {PROJECT} exists without {OWNER_KEY}={OWNER_VALUE}; refusing")
    return found


def ovn_network_config(address):
    return {
        "network": OVN_UPLINK,
        "ipv4.address": address,
        "ipv4.nat": "true",
        "ipv4.dhcp": "true",
        "ipv6.address": "none",
        "dns.nameservers": DNS_NAMESERVERS,
    }


def router_external_ipv4(network):
    config = (network or {}).get("config") or {}
    for key in ("ipv4.nat.address", "volatile.network.ipv4.address"):
        value = config.get(key)
        if value:
            return value
    ovn = (network or {}).get("ovn") or {}
    return ovn.get("uplink_ipv4") or ""


def wait_router_external():
    deadline = time.monotonic() + 30
    last = ""
    while time.monotonic() < deadline:
        network = get_existing(f"/1.0/networks/{NETWORK}?project={PROJECT}")
        external = router_external_ipv4(network)
        if not external:
            state = get_existing(f"/1.0/networks/{NETWORK}/state?project={PROJECT}") or {}
            external = router_external_ipv4(state)
        last = external
        if external:
            return external
        time.sleep(WAIT_IP_POLL)
    return last


def wait_ipv4(name):
    deadline = time.monotonic() + WAIT_IP_SECONDS
    last = None
    while time.monotonic() < deadline:
        state = get_existing(f"/1.0/instances/{name}/state?project={PROJECT}")
        last = state
        if state:
            nic = ((state.get("network") or {}).get("eth0") or {})
            for addr in nic.get("addresses") or []:
                if addr.get("family") == "inet" and addr.get("scope") == "global" and addr.get("address"):
                    return addr["address"]
        time.sleep(WAIT_IP_POLL)
    raise TopologyError(f"instance {name} got no global eth0 IPv4 within {WAIT_IP_SECONDS}s: {last}")


def wait_ping(name, address):
    deadline = time.monotonic() + WAIT_IP_SECONDS
    last = None
    while time.monotonic() < deadline:
        result = guest(name, f"ping -c 1 -W 2 {address}", check=False)
        last = result
        if result.returncode == 0:
            return
        time.sleep(WAIT_IP_POLL)
    detail = ""
    if last is not None:
        detail = (last.stderr or last.stdout or f"exit {last.returncode}").strip()
    raise TopologyError(f"ping {name} -> {address} failed within {WAIT_IP_SECONDS}s: {detail}")


def guest(name, command, check=True):
    return incus(
        "exec",
        remote_prefix() + name,
        "--project",
        PROJECT,
        "--",
        "sh",
        "-c",
        command,
        check=check,
    )


def install_curl(name):
    guest(name, f"printf '%s\\n' '{ALPINE_MAIN}' > /etc/apk/repositories")
    guest(name, "apk add --no-cache curl busybox-extras")


def start_httpd(name):
    script = f"""
set -eu
mkdir -p /srv/www
printf '%s' '{MARKER}' > /srv/www/index.html
dd if=/dev/zero of=/srv/www/large.bin bs=1000000 count=100
busybox-extras httpd -p {FORWARD_PORT} -h /srv/www
sha256sum /srv/www/large.bin
"""
    result = guest(name, script)
    token = result.stdout.strip().split()
    if not token:
        raise TopologyError(f"instance {name} did not print large-file sha256")
    digest = token[0]
    if digest != LARGE_SHA256:
        raise TopologyError(f"large file sha256 {digest} != {LARGE_SHA256}")
    return digest


def launch(name, member, fingerprint):
    query(
        f"/1.0/instances?project={PROJECT}&target={member}",
        "POST",
        {
            "name": name,
            "type": "container",
            "profiles": [],
            "source": {"type": "image", "fingerprint": fingerprint},
            "devices": {
                "root": {"type": "disk", "path": "/", "pool": POOL, "size": ROOT_SIZE},
                "eth0": {"type": "nic", "network": NETWORK, "name": "eth0"},
            },
        },
    )
    incus("start", remote_prefix() + name, "--project", PROJECT)


def cmd_create():
    projects = list_or_absent("/1.0/projects?recursion=1")
    if any(p.get("name") == PROJECT for p in projects):
        raise TopologyError(f"refusing to adopt existing project {PROJECT}")

    uplink = get_existing(f"/1.0/networks/{OVN_UPLINK}?project=default")
    if uplink is None:
        raise TopologyError(f"uplink {OVN_UPLINK} is absent in default project")

    alias = get_existing(f"/1.0/images/aliases/{IMAGE_ALIAS}?project={IMAGE_PROJECT}")
    if alias is None:
        raise TopologyError(f"image alias {IMAGE_ALIAS} absent in {IMAGE_PROJECT}")
    fingerprint = alias["target"]

    query("/1.0/projects", "POST", {"name": PROJECT, "config": project_config()})
    created_at = time.monotonic()
    query(
        f"/1.0/networks?project={PROJECT}",
        "POST",
        {"name": NETWORK, "type": "ovn", "config": ovn_network_config(SUBNET)},
    )
    network_created_at = time.monotonic()
    network_create_seconds = round(network_created_at - created_at, 3)
    network = get_existing(f"/1.0/networks/{NETWORK}?project={PROJECT}")
    if network is None:
        raise TopologyError(f"network {NETWORK} missing after create")
    external = wait_router_external()
    if external == FORWARD_LISTEN:
        raise TopologyError(
            f"router uplink address {external} collides with forward listen {FORWARD_LISTEN}"
        )

    incus(
        "image",
        "copy",
        remote_prefix() + fingerprint,
        remote_prefix(),
        "--project",
        IMAGE_PROJECT,
        "--target-project",
        PROJECT,
        timeout=IMAGE_TIMEOUT,
    )
    launch(INSTANCE_ONE, MEMBER_ONE, fingerprint)
    launch(INSTANCE_TWO, MEMBER_TWO, fingerprint)

    ip_one = wait_ipv4(INSTANCE_ONE)
    ip_two = wait_ipv4(INSTANCE_TWO)
    inst_one = get_existing(f"/1.0/instances/{INSTANCE_ONE}?project={PROJECT}")
    inst_two = get_existing(f"/1.0/instances/{INSTANCE_TWO}?project={PROJECT}")
    loc_one = (inst_one or {}).get("location") or ""
    loc_two = (inst_two or {}).get("location") or ""
    if loc_one != MEMBER_ONE or loc_two != MEMBER_TWO:
        raise TopologyError(f"placement {loc_one}/{loc_two} != {MEMBER_ONE}/{MEMBER_TWO}")

    wait_ping(INSTANCE_ONE, ip_two)
    first_cross_member_ping_seconds = round(time.monotonic() - network_created_at, 3)

    install_curl(INSTANCE_ONE)
    install_curl(INSTANCE_TWO)
    digest = start_httpd(INSTANCE_ONE)
    if start_httpd(INSTANCE_TWO) != digest:
        raise TopologyError("large-file sha256 differed between guests")
    query(
        f"/1.0/networks/{NETWORK}/forwards?project={PROJECT}",
        "POST",
        {
            "listen_address": FORWARD_LISTEN,
            "ports": [
                {
                    "protocol": "tcp",
                    "listen_port": str(FORWARD_PORT),
                    "target_address": ip_one,
                    "target_port": str(FORWARD_PORT),
                }
            ],
        },
    )
    emit(
        {
            "project": PROJECT,
            "network": NETWORK,
            "subnet": SUBNET,
            "uplink": OVN_UPLINK,
            "central": OVN_CENTRAL_ADDRESS,
            "external_ip": external or None,
            "network_create_seconds": network_create_seconds,
            "first_cross_member_ping_seconds": first_cross_member_ping_seconds,
            "instances": {
                INSTANCE_ONE: {"location": loc_one, "ipv4": ip_one},
                INSTANCE_TWO: {"location": loc_two, "ipv4": ip_two},
            },
            "forward": {
                "listen": FORWARD_LISTEN,
                "port": FORWARD_PORT,
                "target": INSTANCE_ONE,
                "target_ipv4": ip_one,
                "marker": MARKER,
                "url": f"http://{FORWARD_LISTEN}:{FORWARD_PORT}/index.html",
            },
            "large_file": {
                "path": "/large.bin",
                "bytes": LARGE_BYTES,
                "sha256": digest,
                "listen_port": FORWARD_PORT,
                "guest_urls": {
                    INSTANCE_ONE: f"http://{ip_one}:{FORWARD_PORT}/large.bin",
                    INSTANCE_TWO: f"http://{ip_two}:{FORWARD_PORT}/large.bin",
                },
            },
        }
    )


def cmd_create_network():
    if require_owned_project() is None:
        raise TopologyError(f"owned project {PROJECT} is absent")
    existing = list_or_absent(f"/1.0/networks?project={PROJECT}&recursion=1")
    if any(n.get("name") == AUX_NETWORK for n in existing):
        raise TopologyError(f"refusing to adopt existing network {AUX_NETWORK}")
    uplink = get_existing(f"/1.0/networks/{OVN_UPLINK}?project=default")
    if uplink is None:
        raise TopologyError(f"uplink {OVN_UPLINK} is absent in default project")
    payload = {"name": AUX_NETWORK, "type": "ovn", "config": ovn_network_config(AUX_SUBNET)}
    started = time.monotonic()
    result = incus(
        "query",
        remote_prefix() + f"/1.0/networks?project={PROJECT}",
        "--request",
        "POST",
        "--wait",
        "--data",
        json.dumps(payload),
        check=False,
        timeout=NETWORK_CREATE_TIMEOUT,
    )
    seconds = round(time.monotonic() - started, 3)
    body = {
        "network": AUX_NETWORK,
        "subnet": AUX_SUBNET,
        "created": result.returncode == 0,
        "code": result.returncode,
        "timeout_seconds": NETWORK_CREATE_TIMEOUT,
        "seconds": seconds,
        "stdout": result.stdout,
        "stderr": result.stderr,
        "error": None if result.returncode == 0 else (result.stderr.strip() or result.stdout.strip() or f"exit {result.returncode}"),
    }
    emit(body, 0 if result.returncode == 0 else 1)


def delete_named_network(name):
    forwards = list_or_absent(f"/1.0/networks/{name}/forwards?project={PROJECT}&recursion=1")
    for forward in forwards:
        listen = forward.get("listen_address")
        if not listen:
            continue
        result = incus(
            "query",
            remote_prefix() + f"/1.0/networks/{name}/forwards/{listen}?project={PROJECT}",
            "--request",
            "DELETE",
            "--wait",
            check=False,
        )
        if result.returncode and not is_absent(result):
            yield f"forward {name}/{listen}: {result.stderr.strip() or result.stdout.strip()}"
    result = incus(
        "query",
        remote_prefix() + f"/1.0/networks/{name}?project={PROJECT}",
        "--request",
        "DELETE",
        "--wait",
        check=False,
    )
    if result.returncode and not is_absent(result):
        yield f"network {name}: {result.stderr.strip() or result.stdout.strip()}"


def cmd_delete_network():
    owned = require_owned_project()
    if owned is None:
        emit({"network": AUX_NETWORK, "deleted": False, "reason": "project absent"})
    failures = list(delete_named_network(AUX_NETWORK))
    emit(
        {"network": AUX_NETWORK, "deleted": not failures, "failures": failures},
        0 if not failures else 1,
    )


def delete_project_images():
    aliases = list_or_absent(f"/1.0/images/aliases?project={PROJECT}&recursion=1")
    for alias in aliases:
        if isinstance(alias, dict):
            name = alias.get("name")
        else:
            name = str(alias).rstrip("/").rsplit("/", 1)[-1]
        if not name:
            continue
        result = incus(
            "image",
            "alias",
            "delete",
            remote_prefix() + name,
            "--project",
            PROJECT,
            check=False,
        )
        if result.returncode and not is_absent(result):
            yield f"image alias {name}: {result.stderr.strip() or result.stdout.strip()}"
    images = list_or_absent(f"/1.0/images?project={PROJECT}&recursion=1")
    for image in images:
        fingerprint = image.get("fingerprint")
        if not fingerprint:
            continue
        result = incus("image", "delete", remote_prefix() + fingerprint, "--project", PROJECT, check=False)
        if result.returncode and not is_absent(result):
            yield f"image {fingerprint}: {result.stderr.strip() or result.stdout.strip()}"


def cmd_destroy():
    projects = list_or_absent("/1.0/projects?recursion=1")
    found = next((p for p in projects if p.get("name") == PROJECT), None)
    if found is None:
        emit({"project": PROJECT, "destroyed": False, "reason": "absent", "failures": []})
    config = found.get("config") or {}
    if config.get(OWNER_KEY) != OWNER_VALUE:
        raise TopologyError(f"project {PROJECT} is not owned ({OWNER_KEY}!={OWNER_VALUE}); refusing destroy")

    failures = []
    instances = list_or_absent(f"/1.0/instances?project={PROJECT}&recursion=1")
    for instance in instances:
        name = instance.get("name")
        if not name:
            continue
        result = incus("delete", "-f", remote_prefix() + name, "--project", PROJECT, check=False)
        if result.returncode and not is_absent(result):
            failures.append(f"instance {name}: {result.stderr.strip() or result.stdout.strip()}")

    networks = list_or_absent(f"/1.0/networks?project={PROJECT}&recursion=1")
    for network in networks:
        name = network.get("name")
        if not name:
            continue
        failures.extend(delete_named_network(name))

    profiles = list_or_absent(f"/1.0/profiles?project={PROJECT}&recursion=1")
    for profile in profiles:
        name = profile.get("name")
        if not name or name == "default":
            continue
        result = incus(
            "query",
            remote_prefix() + f"/1.0/profiles/{name}?project={PROJECT}",
            "--request",
            "DELETE",
            "--wait",
            check=False,
        )
        if result.returncode and not is_absent(result):
            failures.append(f"profile {name}: {result.stderr.strip() or result.stdout.strip()}")

    failures.extend(delete_project_images())

    result = incus("project", "delete", remote_prefix() + PROJECT, check=False)
    if result.returncode and not is_absent(result):
        failures.append(f"project {PROJECT}: {result.stderr.strip() or result.stdout.strip()}")

    emit(
        {"project": PROJECT, "destroyed": not failures, "failures": failures},
        0 if not failures else 1,
    )


def main(argv=None):
    global REMOTE, LOG
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    parser.add_argument("--log", required=True, help="append-only JSONL command log")
    parser.add_argument("--remote", default=REMOTE, help="Incus remote (default nas01)")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("create", help="create owned project, OVN network, guests, forward, HTTP fixture")
    sub.add_parser("destroy", help="destroy owned project only, in dependency order")
    sub.add_parser("create-network", help="create auxiliary recovery network once; no retries")
    sub.add_parser("delete-network", help="delete auxiliary recovery network if present")
    args = parser.parse_args(argv)
    REMOTE = args.remote
    path = Path(args.log)
    path.parent.mkdir(parents=True, exist_ok=True)
    LOG = path.open("a", encoding="utf-8")
    try:
        if args.command == "create":
            cmd_create()
        elif args.command == "destroy":
            cmd_destroy()
        elif args.command == "create-network":
            cmd_create_network()
        elif args.command == "delete-network":
            cmd_delete_network()
        else:
            raise TopologyError(f"unknown command {args.command}")
    except TopologyError as exc:
        log_event({"args": [args.command], "code": 1, "seconds": 0, "stdout": "", "stderr": str(exc), "error": str(exc)})
        print(str(exc), file=sys.stderr)
        emit({"error": str(exc)}, 1)
    finally:
        LOG.close()


if __name__ == "__main__":
    main()
