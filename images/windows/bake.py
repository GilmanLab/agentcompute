#!/usr/bin/env python3
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "PyYAML==6.0.3",
# ]
# ///
"""Bake a Windows image on the Incus cluster and qualify a fresh clone.

Usage:
  uv run --locked --script images/windows/bake.py bake \\
    --image windows-11-desktop --remote nas01 --project ac-win-bake-1 \\
    --target lab01 --pool data --volume-prefix w11 \\
    --evidence-dir /tmp/windows-bake-evidence
  uv run --locked --script images/windows/bake.py promote \\
    --image windows-11-desktop --remote nas01 --fingerprint <fp> \\
    --gui-evidence /tmp/gui-gate.json
  uv run --locked --script images/windows/bake.py teardown \\
    --remote nas01 --project ac-win-bake-1

This process only talks to the Incus API. Windows Setup runs on the cluster's
first-level KVM; nothing here needs nested virtualization, so it is safe to
run from a runner VM with `security.nesting=false`.

`bake` does the whole one-way trip: create the build VM from the staged ISO
volumes, wait for the unattended install and the single first-logon bootstrap,
verify and seal with Sysprep, publish the stopped source, launch a fresh clone
with the runtime device set, qualify it, delete it, and copy the image into the
`image-build` project under a *candidate* alias.

The stable catalog alias is deliberately not touched by `bake`: moving it is
`promote`, which requires a GUI gate evidence file reporting a pass.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

import yaml

WINDOWS_DIR = Path(__file__).resolve().parent
PINS_PATH = WINDOWS_DIR / "pins.lock.yaml"
INSTANCE_PATH = WINDOWS_DIR / "common" / "instance.yaml"
IMAGE_NAMES = ("windows-11-desktop", "windows-server-2025")
STATE_DIR = "C:\\ProgramData\\agentcompute"
STABLE_ALIASES = {
    "windows-11-desktop": "windows/11/desktop",
    "windows-server-2025": "windows/server-2025",
}
PROBE_IMAGE = "images:alpine/3.22"


class Error(RuntimeError):
    """Raised when the bake cannot continue."""


class Evidence:
    """Incremental evidence file: a failed bake still leaves what it proved."""

    def __init__(self, path: Path, header: dict[str, Any]) -> None:
        self.path = path
        self.data: dict[str, Any] = dict(header)
        self.data["phases"] = []
        self.flush()

    def record(self, name: str, detail: Any, seconds: float | None = None) -> Any:
        entry: dict[str, Any] = {"phase": name, "detail": detail}
        if seconds is not None:
            entry["seconds"] = round(seconds, 3)
        entry["at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        self.data["phases"].append(entry)
        self.flush()
        print(f"[{entry['at']}] {name}: {json.dumps(detail)[:400]}")
        return detail

    def set(self, key: str, value: Any) -> None:
        self.data[key] = value
        self.flush()

    def flush(self) -> None:
        self.path.write_text(json.dumps(self.data, indent=2) + "\n", encoding="utf-8")


def load_yaml(path: Path) -> Any:
    return yaml.safe_load(path.read_text(encoding="utf-8"))


def incus(
    args: list[str],
    remote: str | None = None,
    project: str | None = None,
    check: bool = True,
    stdin: str | None = None,
    timeout: int | None = None,
) -> subprocess.CompletedProcess[str]:
    command = ["incus", *args]
    if project:
        command += ["--project", project]
    try:
        # incus reads an instance definition from stdin when stdin is a pipe,
        # so a non-tty parent (a runner job, a supervised process) makes an
        # otherwise fine `incus init` block forever. Feed it nothing.
        result = subprocess.run(
            command, capture_output=True, text=True, timeout=timeout, check=False,
            **({"input": stdin} if stdin is not None else {"stdin": subprocess.DEVNULL}),
        )
    except FileNotFoundError as exc:
        raise Error("incus client not found on PATH") from exc
    except subprocess.TimeoutExpired as exc:
        raise Error(f"incus timed out: {' '.join(command)}") from exc

    if check and result.returncode != 0:
        raise Error(
            f"incus failed ({result.returncode}): {' '.join(command)}\n"
            f"{result.stderr.strip() or result.stdout.strip()}"
        )
    return result


def incus_json(args: list[str], project: str | None = None) -> Any:
    result = incus([*args, "--format", "json"], project=project)
    return json.loads(result.stdout or "null")


def wait_until(
    description: str,
    probe: Any,
    timeout: int,
    interval: float = 5.0,
) -> tuple[Any, float]:
    started = time.monotonic()
    last: Any = None
    while time.monotonic() - started < timeout:
        value = probe()
        if value:
            return value, time.monotonic() - started
        last = value
        time.sleep(interval)

    raise Error(f"timed out after {timeout}s waiting for {description} (last: {last!r})")


def guest_exec(
    instance: str,
    argv: list[str],
    remote: str,
    project: str,
    check: bool = True,
    timeout: int = 900,
) -> subprocess.CompletedProcess[str]:
    return incus(
        ["exec", f"{remote}:{instance}", "--", *argv],
        project=project, check=check, timeout=timeout,
    )


def guest_powershell(
    instance: str, script: str, remote: str, project: str, check: bool = True, timeout: int = 900
) -> str:
    result = guest_exec(
        instance,
        ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script],
        remote, project, check=check, timeout=timeout,
    )
    return result.stdout


def guest_json_file(instance: str, path: str, remote: str, project: str) -> dict[str, Any] | None:
    """Read a JSON report out of the guest through exec.

    PowerShell 5.1 writes UTF-8 with a BOM, and console output can arrive with
    CRLF, so both are stripped before parsing.
    """
    result = guest_powershell(
        instance, f"if (Test-Path '{path}') {{ Get-Content -LiteralPath '{path}' -Raw }}",
        remote, project, check=False,
    )
    text = result.replace("\r\n", "\n").lstrip("\ufeff").strip()
    if not text:
        return None
    try:
        return json.loads(text)
    except json.JSONDecodeError as exc:
        raise Error(f"{path} in {instance} is not valid JSON: {exc}: {text[:200]}") from exc


def instance_state(instance: str, remote: str, project: str) -> dict[str, Any]:
    data = incus_json(["list", f"{remote}:", instance], project=project)
    if not data:
        raise Error(f"instance {instance} not found in project {project}")
    return data[0]


def guest_addresses(instance: str, remote: str, project: str) -> list[str]:
    state = instance_state(instance, remote, project).get("state") or {}
    addresses = []
    for name, nic in (state.get("network") or {}).items():
        if name == "lo":
            continue
        for address in nic.get("addresses") or []:
            if address.get("family") == "inet" and address.get("scope") == "global":
                addresses.append(address["address"])
    return addresses


def agent_ready(instance: str, remote: str, project: str) -> bool:
    result = guest_exec(
        instance, ["cmd.exe", "/c", "echo", "agent-up"], remote, project, check=False, timeout=60
    )
    return result.returncode == 0 and "agent-up" in result.stdout


def create_project(remote: str, project: str, network: str, evidence: Evidence) -> None:
    existing = incus_json(["project", "list", f"{remote}:"])
    if any(entry["name"] == project for entry in existing or []):
        evidence.record("project", {"name": project, "created": False})
        return

    config = {
        "features.images": "true",
        "features.networks": "false",
        "features.profiles": "true",
        "restricted": "true",
        "restricted.cluster.target": "allow",
        "restricted.containers.privilege": "unprivileged",
        "restricted.devices.disk": "managed",
        "restricted.devices.nic": "managed",
        "restricted.networks.access": network,
        "restricted.virtual-machines.nesting": "block",
    }
    incus(["project", "create", f"{remote}:{project}",
           *[f"-c{key}={value}" for key, value in config.items()]])
    evidence.record("project", {"name": project, "created": True, "config": config})


def build_vm(
    image: str,
    settings: dict[str, Any],
    defaults: dict[str, Any],
    args: argparse.Namespace,
    evidence: Evidence,
) -> str:
    name = args.instance or f"{args.volume_prefix}-golden"
    existing = incus_json(["list", f"{args.remote}:", name], project=args.project)
    if existing:
        raise Error(f"instance {name} already exists in {args.project}; pick another name")

    build = settings["build"]
    # The whole instance goes in as one definition rather than an init plus a
    # series of device adds: the disposable project has an empty default
    # profile, so there is no root device to override, and a single definition
    # is also what the VM is reproducible from.
    definition: dict[str, Any] = {
        "config": {
            "limits.cpu": str(build["cpu"]),
            "limits.memory": build["memory"],
            # Incus keys Windows behavior (local-time RTC, Intel IOMMU,
            # disabling unsupported virtual devices) off the image.os prefix.
            "image.os": settings["os"],
            "image.release": str(settings["release"]),
            "image.description": settings["description"],
            **defaults["config"],
        },
        "devices": {
            "root": {
                "type": "disk",
                "path": "/",
                "pool": args.pool,
                "size": build["root_size"],
                # The disk outranks the installer CD. An empty disk has no
                # bootloader, so the firmware falls through to the CD for the
                # first boot; once Setup has applied the image, the disk boots
                # and Setup continues. Booting the CD a second time instead
                # makes Setup find its own staged install and stop on "It
                # looks like you started an upgrade and booted from
                # installation media" - observed live before this ordering.
                "boot.priority": "10",
            },
            "eth0": {"type": "nic", "network": args.network},
            "installer": {
                "type": "disk",
                "pool": args.pool,
                "source": f"{args.volume_prefix}-installer",
                "boot.priority": "1",
            },
            # The agent CD-ROM is how a Windows guest gets incus-agent at all,
            # and it has to stay attached: Incus refreshes the agent and its
            # credentials from it on every boot.
            defaults["agent_device"]["name"]: {
                "type": "disk",
                "source": defaults["agent_device"]["source"],
            },
        },
    }
    if build["tpm"]:
        definition["devices"]["tpm"] = {"type": "tpm"}

    incus(["init", f"{args.remote}:{name}", "--empty", "--vm", "--target", args.target],
          project=args.project, stdin=yaml.safe_dump(definition))

    config = incus(["config", "show", f"{args.remote}:{name}", "--expanded"], project=args.project)
    evidence.record("build-vm", {
        "instance": name,
        "target": args.target,
        "definition": definition,
        "expanded_config": config.stdout,
    })
    return name


def wait_for_install(
    instance: str, args: argparse.Namespace, timeout: int, evidence: Evidence
) -> dict[str, Any]:
    incus(["start", f"{args.remote}:{instance}"], project=args.project)
    started = time.monotonic()

    _, agent_seconds = wait_until(
        "the Incus agent inside the installed guest",
        lambda: agent_ready(instance, args.remote, args.project),
        timeout=timeout, interval=20.0,
    )

    report, report_seconds = wait_until(
        "the first-logon bootstrap report",
        lambda: guest_json_file(instance, f"{STATE_DIR}\\bootstrap-report.json",
                                args.remote, args.project),
        timeout=1800, interval=20.0,
    )

    if report.get("status") != "ok":
        raise Error(f"bootstrap reported {report.get('status')}: {report.get('error')}")

    return evidence.record("install", {
        "install_to_agent_seconds": round(agent_seconds, 1),
        "agent_to_bootstrap_seconds": round(report_seconds, 1),
        "install_to_bootstrap_seconds": round(time.monotonic() - started, 1),
        "bootstrap": report,
    })


def push_guest_scripts(instance: str, args: argparse.Namespace, evidence: Evidence) -> None:
    """Run the working tree's guest scripts, not the ones baked earlier.

    finalize.ps1 and deploy-firstlogon.ps1 are copied into the image during
    bootstrap, so a fix to either would otherwise need a full rebake to take
    effect in the run that is already in flight.
    """
    pushed = {}
    for script in ("finalize.ps1", "deploy-firstlogon.ps1", "smoke.ps1"):
        result = incus(
            ["file", "push", str(WINDOWS_DIR / "common" / script),
             f"{args.remote}:{instance}/C:/ProgramData/agentcompute/{script}"],
            project=args.project, check=False, timeout=180,
        )
        pushed[script] = {"exit": result.returncode, "stderr": result.stderr.strip()}

    evidence.record("guest-scripts-push", pushed)


def finalize_and_seal(
    instance: str, args: argparse.Namespace, evidence: Evidence
) -> dict[str, Any]:
    push_guest_scripts(instance, args, evidence)
    verify = guest_exec(
        instance,
        ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass",
         "-File", f"{STATE_DIR}\\finalize.ps1", "-Phase", "verify"],
        args.remote, args.project, check=False, timeout=2400,
    )
    report = guest_json_file(instance, f"{STATE_DIR}\\finalize-report.json",
                             args.remote, args.project)
    if verify.returncode != 0 or not report or report.get("status") != "ok":
        raise Error(
            f"finalize verify failed (exit {verify.returncode}): "
            f"{(report or {}).get('error') or verify.stderr.strip()}"
        )

    evidence.record("finalize-verify", report)

    # Setup media leaves before the capture: the installer volume carries the
    # answer file and the pinned installers, and a captured image must not ship
    # either.
    incus(["config", "device", "remove", f"{args.remote}:{instance}", "installer"],
          project=args.project)

    seal_started = time.monotonic()
    # Sysprep powers the VM off, so the exec channel dies mid-call by design.
    guest_exec(
        instance,
        ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass",
         "-File", f"{STATE_DIR}\\finalize.ps1", "-Phase", "seal"],
        args.remote, args.project, check=False, timeout=600,
    )

    _, stop_seconds = wait_until(
        "the generalized source to power off",
        lambda: instance_state(instance, args.remote, args.project)["status"] == "Stopped",
        timeout=1800, interval=10.0,
    )

    return evidence.record("seal", {
        "sysprep_to_stopped_seconds": round(stop_seconds, 1),
        "total_seal_seconds": round(time.monotonic() - seal_started, 1),
        "media_detached": ["installer"],
        "warning": "the generalized source must never be booted again",
    })


def publish_candidate(
    instance: str, image: str, args: argparse.Namespace, evidence: Evidence
) -> dict[str, Any]:
    alias = f"{args.volume_prefix}-candidate"
    incus(["publish", f"{args.remote}:{instance}", "--alias", alias],
          project=args.project, timeout=7200)
    info = incus_json(["image", "list", f"{args.remote}:", alias], project=args.project)
    if not info:
        raise Error(f"publish produced no image for alias {alias}")

    return evidence.record("publish", {
        "alias": alias,
        "fingerprint": info[0]["fingerprint"],
        "size_bytes": info[0]["size"],
        "properties": info[0].get("properties"),
    })


def launch_clone(
    fingerprint: str,
    settings: dict[str, Any],
    defaults: dict[str, Any],
    args: argparse.Namespace,
    evidence: Evidence,
) -> str:
    name = f"{args.volume_prefix}-clone"
    runtime = settings["runtime"]
    # The clone gets the runtime device set, not the build one: no installer
    # media, but the same agent CD, vTPM and Secure Boot a real sandbox
    # instance would get.
    definition: dict[str, Any] = {
        "config": {
            "limits.cpu": str(runtime["cpu"]),
            "limits.memory": runtime["memory"],
            **defaults["config"],
        },
        "devices": {
            "root": {"type": "disk", "path": "/", "pool": args.pool},
            "eth0": {"type": "nic", "network": args.network},
            defaults["agent_device"]["name"]: {
                "type": "disk",
                "source": defaults["agent_device"]["source"],
            },
        },
    }
    if runtime["tpm"]:
        definition["devices"]["tpm"] = {"type": "tpm"}

    incus(["init", f"{args.remote}:{fingerprint}", f"{args.remote}:{name}", "--vm",
           "--target", args.target],
          project=args.project, stdin=yaml.safe_dump(definition))
    incus(["start", f"{args.remote}:{name}"], project=args.project)
    evidence.record("clone-start", {
        "instance": name, "fingerprint": fingerprint, "definition": definition,
    })
    return name


def probe_container(args: argparse.Namespace, evidence: Evidence) -> str:
    """A same-subnet prober.

    The image's firewall rule allows VNC only from LocalSubnet, so the honest
    way to test the fallback console is from another instance on the sandbox
    network, not from an external forward.
    """
    name = f"{args.volume_prefix}-probe"
    existing = incus_json(["list", f"{args.remote}:", name], project=args.project)
    if not existing:
        incus(["launch", PROBE_IMAGE, f"{args.remote}:{name}", "--target", args.target],
              project=args.project, stdin=yaml.safe_dump({
                  "devices": {
                      "root": {"type": "disk", "path": "/", "pool": args.pool},
                      "eth0": {"type": "nic", "network": args.network},
                  },
              }), timeout=600)
        wait_until(
            "the prober container to get an address",
            lambda: guest_addresses(name, args.remote, args.project),
            timeout=180, interval=5.0,
        )

    evidence.record("probe-container", {
        "instance": name,
        "addresses": guest_addresses(name, args.remote, args.project),
    })
    return name


def vnc_banner(probe: str, address: str, port: int, args: argparse.Namespace) -> str:
    result = incus(
        ["exec", f"{args.remote}:{probe}", "--", "sh", "-c",
         f"printf '' | nc -w 5 {address} {port} | head -c 12 | tr -d '\\n'"],
        project=args.project, check=False, timeout=60,
    )
    return result.stdout.strip()


def qualify_clone(
    clone: str,
    image: str,
    settings: dict[str, Any],
    golden_identity: dict[str, Any],
    args: argparse.Namespace,
    evidence: Evidence,
) -> dict[str, Any]:
    role = "desktop" if settings["desktop"] else "server-core"
    runtime = settings["runtime"]

    _, ready_seconds = wait_until(
        "the clone's Incus agent",
        lambda: agent_ready(clone, args.remote, args.project),
        timeout=runtime["ready_timeout"], interval=10.0,
    )

    ver = guest_exec(clone, ["cmd.exe", "/c", "ver"], args.remote, args.project)

    # Qualification tooling comes from the working tree, not from the image, so
    # a gate change does not require a rebake. The image keeps its own copy for
    # later use; this push makes the run authoritative.
    pushed = incus(
        ["file", "push", str(WINDOWS_DIR / "common" / "smoke.ps1"),
         f"{args.remote}:{clone}/C:/ProgramData/agentcompute/smoke.ps1"],
        project=args.project, check=False, timeout=120,
    )
    evidence.record("smoke-script-push", {
        "exit": pushed.returncode,
        "stderr": pushed.stderr.strip(),
        "source": "working tree" if pushed.returncode == 0 else "image copy (push failed)",
    })

    smoke_started = time.monotonic()
    smoke = guest_exec(
        clone,
        ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass",
         "-File", f"{STATE_DIR}\\smoke.ps1", "-Role", role,
         "-VncPort", str(runtime.get("vnc_port", 5900))],
        args.remote, args.project, check=False, timeout=1200,
    )
    text = smoke.stdout.replace("\r\n", "\n").strip().splitlines()
    parsed: dict[str, Any] | None = None
    for line in reversed(text):
        if line.startswith("{"):
            parsed = json.loads(line)
            break

    if parsed is None:
        raise Error(f"clone smoke produced no JSON: {smoke.stdout[:500]} {smoke.stderr[:500]}")

    identity = parsed["identity"]
    fresh_sid = identity["machine_sid"] != golden_identity.get("machine_sid")
    fresh_name = identity["computer_name"] != golden_identity.get("computer_name")

    # The binary file API is the transport the server uses for screenshots, so
    # prove it works on a Windows guest rather than assuming it.
    pulled = Path(args.evidence_dir) / f"{clone}-deploy-report.json"
    file_api = incus(
        ["file", "pull", f"{args.remote}:{clone}/C:/ProgramData/agentcompute/deploy-report.json",
         str(pulled)],
        project=args.project, check=False, timeout=120,
    )

    # Pull the full-desktop capture the guest just measured, both as visual
    # evidence and as a real test of the screenshot path the server uses.
    screenshot: dict[str, Any] | None = None
    for check in parsed["checks"]:
        if check["name"] != "desktop-screenshot-not-black":
            continue
        detail = check.get("detail") or {}
        guest_path = detail.get("path")
        if not guest_path:
            break
        local = Path(args.evidence_dir) / f"{clone}-desktop.png"
        pull = incus(
            ["file", "pull",
             f"{args.remote}:{clone}/" + guest_path.replace("\\", "/"), str(local)],
            project=args.project, check=False, timeout=180,
        )
        screenshot = {
            "guest_path": guest_path,
            "local_path": str(local),
            "pull_exit": pull.returncode,
            "pull_error": pull.stderr.strip(),
            "local_size": local.stat().st_size if local.is_file() else None,
            "measured": {key: value for key, value in detail.items() if key != "path"},
        }
        break

    result: dict[str, Any] = {
        "role": role,
        "boot_to_agent_seconds": round(ready_seconds, 1),
        "cmd_ver": ver.stdout.strip(),
        "smoke_seconds": round(time.monotonic() - smoke_started, 1),
        "smoke": parsed,
        "fresh_machine_sid": fresh_sid,
        "fresh_computer_name": fresh_name,
        "golden_identity": golden_identity,
        "file_api": {
            "command": "incus file pull <clone>/C:/ProgramData/agentcompute/deploy-report.json",
            "exit": file_api.returncode,
            "stderr": file_api.stderr.strip(),
            "local_size": pulled.stat().st_size if pulled.is_file() else None,
        },
        "desktop_screenshot": screenshot,
    }

    if parsed["status"] != "ok" or not fresh_sid or not fresh_name:
        evidence.record("clone-qualification", result)
        raise Error(
            f"clone qualification failed: status={parsed['status']} "
            f"failed={parsed.get('failed')} fresh_sid={fresh_sid} fresh_name={fresh_name}"
        )

    if role == "desktop":
        result["vnc_at_login_screen"] = vnc_login_screen_gate(clone, runtime, args, evidence)

    return evidence.record("clone-qualification", result)


def vnc_login_screen_gate(
    clone: str, runtime: dict[str, Any], args: argparse.Namespace, evidence: Evidence
) -> dict[str, Any]:
    """Prove the fallback console answers with nobody logged on.

    Auto-logon normally consumes the login screen within seconds, so it is
    switched off, the clone is rebooted, the absence of an interactive session
    is confirmed, and only then is the banner read. Auto-logon is restored and
    the Driver daemon re-checked afterwards, which also measures reboot
    recovery.
    """
    port = runtime.get("vnc_port", 5900)
    probe = probe_container(args, evidence)

    logon_key = 'HKLM:\\SOFTWARE\\Microsoft\\Windows NT\\CurrentVersion\\Winlogon'
    guest_powershell(clone, f"Set-ItemProperty -Path '{logon_key}' -Name AutoAdminLogon -Value 0",
                     args.remote, args.project)
    incus(["restart", f"{args.remote}:{clone}"], project=args.project, timeout=600)
    wait_until("the clone's agent after the no-autologon reboot",
               lambda: agent_ready(clone, args.remote, args.project),
               timeout=runtime["ready_timeout"], interval=10.0)

    sessions = guest_powershell(
        clone,
        "$s = Get-CimInstance Win32_LogonSession -Filter 'LogonType=2';"
        " if ($s) { ($s | ForEach-Object { $_.LogonId }) -join ',' } else { 'none' }",
        args.remote, args.project,
    ).strip()

    addresses = guest_addresses(clone, args.remote, args.project)
    if not addresses:
        raise Error("clone has no global IPv4 address; cannot probe VNC")

    banner, banner_seconds = wait_until(
        "the VNC banner with nobody logged on",
        lambda: vnc_banner(probe, addresses[0], port, args) or None,
        timeout=180, interval=5.0,
    )

    guest_powershell(clone, f"Set-ItemProperty -Path '{logon_key}' -Name AutoAdminLogon -Value 1",
                     args.remote, args.project)
    incus(["restart", f"{args.remote}:{clone}"], project=args.project, timeout=600)
    wait_until("the clone's agent after restoring autologon",
               lambda: agent_ready(clone, args.remote, args.project),
               timeout=runtime["ready_timeout"], interval=10.0)

    driver_status, driver_seconds = wait_until(
        "the Driver daemon after the autologon reboot",
        lambda: (lambda text: text if "running" in text else None)(
            guest_powershell(
                clone, f"& '{STATE_DIR}\\cua-driver\\cua-driver.exe' status 2>&1 | Out-String",
                args.remote, args.project, check=False,
            )
        ),
        timeout=600, interval=10.0,
    )

    return {
        "probe_instance": probe,
        "clone_address": addresses[0],
        "port": port,
        "interactive_sessions_while_probing": sessions,
        "banner": banner,
        "banner_wait_seconds": round(banner_seconds, 1),
        "driver_after_reboot": driver_status.strip(),
        "driver_recovery_seconds": round(driver_seconds, 1),
    }


def copy_to_image_build(
    fingerprint: str, image: str, args: argparse.Namespace, evidence: Evidence
) -> dict[str, Any]:
    media = load_yaml(PINS_PATH)["media"][image]
    alias = f"{STABLE_ALIASES[image]}-candidate-{media['build']}"
    if args.project == args.promotion_project:
        # The bake already ran in the promotion project, which is the only
        # project a narrowly scoped publisher certificate may reach. Nothing to
        # copy: just name the fingerprint.
        incus(["image", "alias", "create", f"{args.remote}:{alias}", fingerprint],
              project=args.promotion_project)
    else:
        incus(["image", "copy", f"{args.remote}:{fingerprint}", f"{args.remote}:",
               "--target-project", args.promotion_project, "--alias", alias],
              project=args.project, timeout=7200)

    info = incus_json(["image", "list", f"{args.remote}:", alias], project=args.promotion_project)
    if not info:
        raise Error(f"image copy to {args.promotion_project} produced no {alias}")

    for key, value in {
        "os": media["product"],
        "release": str(media.get("release", media["build"])),
        "agentcompute.build": media["build"],
        "agentcompute.media_provenance": media["provenance"],
        "agentcompute.gate": "candidate-awaiting-gui-gate",
    }.items():
        incus(["image", "set-property", f"{args.remote}:{alias}", key, value],
              project=args.promotion_project)

    return evidence.record("candidate-alias", {
        "project": args.promotion_project,
        "alias": alias,
        "fingerprint": info[0]["fingerprint"],
        "size_bytes": info[0]["size"],
        "stable_alias_moved": False,
        "note": "the stable alias is moved by `promote` only, after the GUI gate passes",
    })


def bake(args: argparse.Namespace) -> int:
    pins = load_yaml(PINS_PATH)
    media = pins["media"][args.image]
    if media["gate"] != "cleared":
        raise Error(f"{args.image} media gate is {media['gate']}: {media.get('gate_reason')}")

    instance_settings = load_yaml(INSTANCE_PATH)
    settings = instance_settings["images"][args.image]
    defaults = instance_settings["defaults"]

    evidence_dir = Path(args.evidence_dir).expanduser()
    evidence_dir.mkdir(parents=True, exist_ok=True)
    evidence = Evidence(evidence_dir / f"{args.image}-bake.json", {
        "schema_version": 1,
        "image": args.image,
        "media": {
            "product": media["product"],
            "build": media["build"],
            "sha256": media["sha256"],
            "provenance": media["provenance"],
        },
        "remote": args.remote,
        "project": args.project,
        "target": args.target,
        "pool": args.pool,
        "network": args.network,
        "started": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    })

    started = time.monotonic()
    if args.create_project:
        create_project(args.remote, args.project, args.network, evidence)

    golden = build_vm(args.image, settings, defaults, args, evidence)
    install = wait_for_install(golden, args, settings["build"]["install_timeout"], evidence)
    golden_identity = {
        "computer_name": install["bootstrap"]["facts"]["os"].get("computer_name")
        or guest_powershell(golden, "$env:COMPUTERNAME", args.remote, args.project).strip(),
        "machine_sid": guest_powershell(
            golden, "(Get-LocalUser -Name 'Administrator').SID.AccountDomainSid.Value",
            args.remote, args.project,
        ).strip(),
    }
    evidence.record("golden-identity", golden_identity)

    finalize_and_seal(golden, args, evidence)
    published = publish_candidate(golden, args.image, args, evidence)

    clone = launch_clone(published["fingerprint"], settings, defaults, args, evidence)
    try:
        qualify_clone(clone, args.image, settings, golden_identity, args, evidence)
    finally:
        if not args.keep_clone:
            incus(["delete", f"{args.remote}:{clone}", "--force"],
                  project=args.project, check=False)
            evidence.record("clone-deleted", {"instance": clone})

    candidate = copy_to_image_build(published["fingerprint"], args.image, args, evidence)

    evidence.set("status", "ok")
    evidence.set("total_seconds", round(time.monotonic() - started, 1))
    evidence.set("promotion_gate", {
        "stable_alias": STABLE_ALIASES[args.image],
        "candidate_alias": candidate["alias"],
        "remaining": "GUI gate through desktop.call on a fresh clone, then `promote`",
    })
    print(json.dumps(evidence.data["promotion_gate"], indent=2))
    return 0


def promote(args: argparse.Namespace) -> int:
    """Move the stable alias, bound to one image and one fingerprint.

    The gate evidence is not a bare pass flag: it has to name the same image
    and the same fingerprint being promoted, so a stale or unrelated
    qualification cannot move an alias. The desktop image's gate includes the
    GUI call; Server Core's is its own headless qualification. The alias is
    updated in place with a single PUT rather than delete-then-create, so an
    interrupted promotion cannot leave the catalog with no stable alias.
    """
    evidence_path = Path(args.qualification_evidence)
    gate = json.loads(evidence_path.read_text(encoding="utf-8"))
    if gate.get("status") != "pass":
        raise Error(
            f"{evidence_path} reports status {gate.get('status')!r}; "
            "the stable alias only moves on a qualification pass"
        )

    if gate.get("image") != args.image:
        raise Error(
            f"{evidence_path} qualifies image {gate.get('image')!r}, not {args.image!r}"
        )

    if gate.get("fingerprint") != args.fingerprint:
        raise Error(
            f"{evidence_path} qualifies fingerprint {gate.get('fingerprint')!r}, "
            f"not {args.fingerprint!r}"
        )

    present = incus_json(["image", "list", f"{args.remote}:", args.fingerprint],
                         project=args.promotion_project)
    if not present or present[0]["fingerprint"] != args.fingerprint:
        raise Error(
            f"image {args.fingerprint} is not in project {args.promotion_project}; "
            "copy the candidate there before promoting"
        )

    alias = STABLE_ALIASES[args.image]
    existing = incus_json(["image", "alias", "list", f"{args.remote}:", alias],
                          project=args.promotion_project)
    previous = existing[0]["target"] if existing else None
    if previous == args.fingerprint:
        print(json.dumps({"alias": alias, "fingerprint": args.fingerprint, "changed": False}))
        return 0

    if existing:
        # Atomic retarget through the API: the alias never stops existing.
        incus(["query", "-X", "PUT",
               f"{args.remote}:/1.0/images/aliases/{alias}?project={args.promotion_project}",
               "-d", json.dumps({"target": args.fingerprint,
                                 "description": existing[0].get("description", "")})])
    else:
        incus(["image", "alias", "create", f"{args.remote}:{alias}", args.fingerprint],
              project=args.promotion_project)

    rollback = None
    if previous:
        rollback = (
            f"incus query -X PUT {args.remote}:/1.0/images/aliases/{alias}"
            f"?project={args.promotion_project} -d '{{\"target\":\"{previous}\"}}'"
        )

    print(json.dumps({
        "alias": alias,
        "fingerprint": args.fingerprint,
        "previous_fingerprint": previous,
        "qualification_evidence": str(evidence_path),
        "changed": True,
        "rollback": rollback,
    }, indent=2))
    return 0


def teardown(args: argparse.Namespace) -> int:
    removed: dict[str, list[str]] = {"instances": [], "images": [], "volumes": []}
    for entry in incus_json(["list", f"{args.remote}:"], project=args.project) or []:
        incus(["delete", f"{args.remote}:{entry['name']}", "--force"], project=args.project)
        removed["instances"].append(entry["name"])

    for entry in incus_json(["image", "list", f"{args.remote}:"], project=args.project) or []:
        incus(["image", "delete", f"{args.remote}:{entry['fingerprint']}"], project=args.project)
        removed["images"].append(entry["fingerprint"])

    for entry in incus_json(["storage", "volume", "list", f"{args.remote}:{args.pool}"],
                            project=args.project) or []:
        if entry.get("type") != "custom":
            continue
        incus(["storage", "volume", "delete", f"{args.remote}:{args.pool}", entry["name"]],
              project=args.project)
        removed["volumes"].append(entry["name"])

    if args.delete_project:
        incus(["project", "delete", f"{args.remote}:{args.project}"])
        removed["project"] = [args.project]

    print(json.dumps(removed, indent=2))
    return 0


def project(args: argparse.Namespace) -> int:
    """Create the disposable build project.

    The ISO volumes have to be imported into the project before the bake can
    create a VM that boots them, so project creation is available on its own.
    """
    evidence_dir = Path(args.evidence_dir).expanduser()
    evidence_dir.mkdir(parents=True, exist_ok=True)
    evidence = Evidence(evidence_dir / f"{args.project}-project.json", {
        "schema_version": 1,
        "remote": args.remote,
        "project": args.project,
    })
    create_project(args.remote, args.project, args.network, evidence)
    return 0



def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    bake_cmd = sub.add_parser("bake", help="install, seal, capture and qualify")
    bake_cmd.add_argument("--image", choices=IMAGE_NAMES, required=True)
    bake_cmd.add_argument("--remote", default="local")
    bake_cmd.add_argument("--project", required=True, help="disposable build project")
    bake_cmd.add_argument("--promotion-project", default="image-build")
    bake_cmd.add_argument("--target", required=True, help="cluster member running the bake")
    bake_cmd.add_argument("--pool", default="data")
    bake_cmd.add_argument("--network", default="incusbr0")
    bake_cmd.add_argument("--volume-prefix", required=True)
    bake_cmd.add_argument("--instance", help="golden source instance name")
    bake_cmd.add_argument("--evidence-dir", required=True)
    bake_cmd.add_argument("--create-project", action="store_true")
    bake_cmd.add_argument("--keep-clone", action="store_true",
                          help="leave the qualification clone running for an external GUI gate")


    project_cmd = sub.add_parser("project", help="create the disposable build project")
    project_cmd.add_argument("--remote", default="local")
    project_cmd.add_argument("--project", required=True)
    project_cmd.add_argument("--network", default="incusbr0")
    project_cmd.add_argument("--evidence-dir", required=True)

    promote_cmd = sub.add_parser("promote", help="move the stable alias after the GUI gate")
    promote_cmd.add_argument("--image", choices=IMAGE_NAMES, required=True)
    promote_cmd.add_argument("--remote", default="local")
    promote_cmd.add_argument("--promotion-project", default="image-build")
    promote_cmd.add_argument("--fingerprint", required=True)
    promote_cmd.add_argument(
        "--qualification-evidence", required=True,
        help="JSON file with status 'pass' plus the image and fingerprint it qualifies",
    )

    teardown_cmd = sub.add_parser("teardown", help="remove everything the bake created")
    teardown_cmd.add_argument("--remote", default="local")
    teardown_cmd.add_argument("--project", required=True)
    teardown_cmd.add_argument("--pool", default="data")
    teardown_cmd.add_argument("--delete-project", action="store_true")

    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    try:
        if args.command == "bake":
            return bake(args)
        if args.command == "project":
            return project(args)
        if args.command == "promote":
            return promote(args)
        return teardown(args)
    except Error as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
