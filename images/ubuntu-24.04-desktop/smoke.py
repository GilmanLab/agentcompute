#!/usr/bin/env python3
"""Boot-test a split desktop VM for X11, cua-driver, list_apps, and live capture.

The instance is created with security.nesting=false. This script imports the
candidate only when the fingerprint is absent, never touches shared aliases,
and deletes only the instance and image this run created.

Usage:
  python3 images/ubuntu-24.04-desktop/smoke.py \\
    --metadata PATH/incus.tar.xz --disk PATH/disk.qcow2 \\
    --remote nas01 --project image-build --profile runner-smoke \\
    --evidence DIR
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

DRIVER_BIN = "/usr/local/bin/cua-driver"
DRIVER_SOCKET = "/run/user/1000/cua-driver.sock"
GUEST_HOME = "/home/automation"
RUNTIME_DIR = "/run/user/1000"
GUEST_UID = "1000"
GUEST_GID = "1000"
X11_SOCKET = "/tmp/.X11-unix/X0"
EDITOR_LAUNCH = "gnome-text-editor"
GUEST_DESKTOP_PNG = "/tmp/agentcompute-desktop-smoke.png"


class Error(RuntimeError):
    """Smoke failed."""


def note(message: str, log: Path) -> None:
    line = f"desktop-smoke: {message}"
    print(line, file=sys.stderr)
    with log.open("a", encoding="utf-8") as handle:
        handle.write(line + "\n")


def run(args: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, text=True, **kwargs)  # type: ignore[arg-type]


def incus(project: str, args: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
    return run(["incus", "--quiet", "--project", project, *args], **kwargs)


def die(message: str) -> None:
    raise Error(message)


def wait_until(deadline: float, description: str, check: object) -> None:
    while time.time() < deadline:
        if check():  # type: ignore[misc]
            return
        time.sleep(2)
    die(f"timed out waiting for {description}")


def instance_config(remote: str, project: str, name: str) -> dict[str, object]:
    result = run(
        ["incus", "query", f"{remote}:/1.0/instances/{name}?project={project}"],
        capture_output=True,
    )
    if result.returncode != 0:
        die(f"instance query failed: {(result.stderr or result.stdout or '').strip()}")
    try:
        data = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        die(f"instance query returned non-JSON: {exc}")
    if not isinstance(data, dict):
        die("instance query returned a non-object")
    return data


def nesting_enabled(config: dict[str, object]) -> bool:
    value = config.get("config")
    if not isinstance(value, dict):
        return False
    nesting = str(value.get("security.nesting") or "false").strip().lower()
    return nesting not in {"false", "0", ""}


def guest_exec(
    remote: str,
    project: str,
    name: str,
    command: list[str],
    *,
    user: bool = False,
) -> subprocess.CompletedProcess[str]:
    args = ["exec", f"{remote}:{name}"]
    if user:
        args.extend(
            [
                "--user",
                GUEST_UID,
                "--group",
                GUEST_GID,
                "--cwd",
                GUEST_HOME,
                "--env",
                f"HOME={GUEST_HOME}",
                "--env",
                f"XDG_RUNTIME_DIR={RUNTIME_DIR}",
                "--env",
                f"DBUS_SESSION_BUS_ADDRESS=unix:path={RUNTIME_DIR}/bus",
            ]
        )
    args.extend(["--", *command])
    return incus(project, args, capture_output=True)


def guest_pull(remote: str, project: str, name: str, guest_path: str, target: Path) -> None:
    pulled = incus(
        project,
        ["file", "pull", f"{remote}:{name}{guest_path}", str(target)],
        capture_output=True,
    )
    if pulled.returncode != 0:
        die(f"pulling {guest_path} failed: {(pulled.stderr or pulled.stdout or '').strip()}")


def screenshot_pixels(raw: bytes) -> bytes:
    """Compare PNG image data without timestamps or other ancillary metadata."""
    if not raw.startswith(b"\x89PNG\r\n\x1a\n"):
        die("screenshot is not a PNG")
    offset = 8
    pixels = bytearray()
    while offset + 12 <= len(raw):
        length = int.from_bytes(raw[offset : offset + 4], "big")
        end = offset + 12 + length
        if end > len(raw):
            die("screenshot PNG contains a truncated chunk")
        if raw[offset + 4 : offset + 8] == b"IDAT":
            pixels.extend(raw[offset + 8 : end - 4])
        offset = end
    if not pixels:
        die("screenshot PNG contains no image data")
    return bytes(pixels)


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--metadata", type=Path, required=True)
    parser.add_argument("--disk", type=Path, required=True)
    parser.add_argument("--remote", required=True)
    parser.add_argument("--project", required=True)
    parser.add_argument("--profile", required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--timeout", type=int, default=720)
    args = parser.parse_args(argv)

    metadata = args.metadata.expanduser().resolve()
    disk = args.disk.expanduser().resolve()
    evidence = args.evidence.expanduser()
    if not evidence.is_absolute():
        evidence = Path.cwd() / evidence
    remote = args.remote
    project = args.project
    profile = args.profile
    timeout = args.timeout

    if project == "default":
        die("--project must not be the default project")
    if not metadata.is_file():
        die(f"metadata not found: {metadata}")
    if not disk.is_file():
        die(f"disk not found: {disk}")
    if timeout <= 0:
        die("--timeout must be a positive number of seconds")
    if shutil.which("incus") is None:
        die("incus client not found on PATH")

    evidence.mkdir(parents=True, exist_ok=True)
    log = evidence / "smoke.log"
    log.write_text("", encoding="utf-8")
    suffix = time.strftime("%Y%m%d%H%M%S", time.gmtime()) + f"-{os.getpid()}"
    name = f"desktop-smoke-{suffix}"
    if len(name) > 63:
        die(f"derived name is longer than 63 characters: {name}")

    imported = False
    created = False
    fingerprint = ""

    def cleanup() -> None:
        failures = 0
        if created:
            note(f"deleting instance {name}", log)
            result = incus(project, ["delete", "-f", f"{remote}:{name}"])
            failures += 0 if result.returncode == 0 else 1
        if imported and fingerprint:
            note(f"deleting image {fingerprint}", log)
            result = incus(project, ["image", "delete", f"{remote}:{fingerprint}"])
            failures += 0 if result.returncode == 0 else 1
        if failures:
            die("cleanup failed; the project may need manual inspection")

    exit_code = 0
    try:
        shown = incus(project, ["profile", "show", f"{remote}:{profile}"], capture_output=True)
        if shown.returncode != 0:
            die(f"profile {profile} is missing in {remote} project {project}")

        hasher = hashlib.sha256()
        for artifact in (metadata, disk):
            with artifact.open("rb") as source:
                while block := source.read(1024 * 1024):
                    hasher.update(block)
        fingerprint = hasher.hexdigest()
        listed = incus(project, ["image", "list", f"{remote}:", "--format=json"], capture_output=True)
        if listed.returncode:
            die(f"image inventory failed: {listed.stderr}")
        present = any(image["fingerprint"] == fingerprint for image in json.loads(listed.stdout))
        if not present:
            note(f"importing {metadata.name} + {disk.name}", log)
            imported_run = incus(
                project,
                ["image", "import", str(metadata), str(disk), f"{remote}:"],
                capture_output=True,
            )
            if imported_run.returncode != 0:
                die(f"image import failed: {imported_run.stderr}")
            imported = True
            verified = incus(project, ["image", "info", f"{remote}:{fingerprint}"], capture_output=True)
            if verified.returncode:
                die("import did not produce the fingerprint of the verified metadata and disk")
        note(f"verified fingerprint {fingerprint}; smoke owns image={imported}", log)

        note(f"creating {name} --vm --profile {profile}", log)
        init = incus(
            project,
            [
                "init",
                f"{remote}:{fingerprint}",
                f"{remote}:{name}",
                "--vm",
                "--profile",
                profile,
                "-c",
                "limits.cpu=4",
                "-c",
                "limits.memory=8192MiB",
            ],
            capture_output=True,
        )
        if init.returncode != 0:
            die(f"init failed: {(init.stderr or init.stdout or '').strip()}")
        created = True
        nested = incus(
            project,
            ["config", "set", f"{remote}:{name}", "security.nesting=false"],
            capture_output=True,
        )
        if nested.returncode != 0 and nesting_enabled(instance_config(remote, project, name)):
            die(f"security.nesting=false rejected: {(nested.stderr or nested.stdout or '').strip()}")
        if nesting_enabled(instance_config(remote, project, name)):
            die("security.nesting must be false")
        start = incus(project, ["start", f"{remote}:{name}"], capture_output=True)
        if start.returncode != 0:
            die(f"start failed: {(start.stderr or start.stdout or '').strip()}")

        deadline = time.time() + timeout
        note("waiting for incus-agent", log)

        def agent_up() -> bool:
            return guest_exec(remote, project, name, ["/bin/true"]).returncode == 0

        wait_until(deadline, "incus-agent", agent_up)

        def x11_ready() -> bool:
            return guest_exec(remote, project, name, ["test", "-S", X11_SOCKET]).returncode == 0

        wait_until(deadline, "X11 display socket", x11_ready)
        note("X11 display socket is present", log)

        def driver_socket_ready() -> bool:
            return guest_exec(remote, project, name, ["test", "-S", DRIVER_SOCKET]).returncode == 0

        wait_until(deadline, "cua-driver socket", driver_socket_ready)
        note("cua-driver socket is present", log)

        def unit_active() -> bool:
            result = guest_exec(
                remote,
                project,
                name,
                ["systemctl", "--user", "--machine=automation@", "is-active", "cua-driver.service"],
            )
            return (result.stdout or "").strip() == "active"

        wait_until(deadline, "cua-driver user unit", unit_active)
        note("cua-driver.service is active for automation", log)

        listed_apps = guest_exec(
            remote,
            project,
            name,
            [DRIVER_BIN, "call", "--socket", DRIVER_SOCKET, "list_apps", "{}"],
            user=True,
        )
        stdout = listed_apps.stdout or ""
        (evidence / "list-apps.stdout").write_text(stdout, encoding="utf-8")
        (evidence / "list-apps.stderr").write_text(listed_apps.stderr or "", encoding="utf-8")
        if listed_apps.returncode != 0:
            die(f"list_apps exited {listed_apps.returncode}: {stdout}{listed_apps.stderr}")
        try:
            payload = json.loads(stdout)
        except json.JSONDecodeError:
            die(f"list_apps returned non-JSON; refusing success: {stdout[:500]}")
        if not isinstance(payload, dict):
            die("list_apps JSON was not an object")
        apps = payload.get("apps")
        if not isinstance(apps, list) or not apps:
            die("list_apps did not return a non-empty apps list")
        if not any(isinstance(app, dict) and app.get("launch_path") == EDITOR_LAUNCH for app in apps):
            die("list_apps did not include gnome-text-editor")
        note(f"list_apps returned {len(apps)} apps including {EDITOR_LAUNCH}", log)

        def capture(filename: str) -> bytes:
            captured = guest_exec(
                remote, project, name,
                [
                    DRIVER_BIN, "call", "--socket", DRIVER_SOCKET,
                    "--screenshot-out-file", GUEST_DESKTOP_PNG,
                    "get_desktop_state", "{}",
                ],
                user=True,
            )
            if captured.returncode != 0:
                die(f"get_desktop_state exited {captured.returncode}: {captured.stderr}")
            target = evidence / filename
            guest_pull(remote, project, name, GUEST_DESKTOP_PNG, target)
            return screenshot_pixels(target.read_bytes())

        before = capture("desktop-before.png")
        launched = guest_exec(
            remote, project, name,
            [
                "systemd-run", "--user", "--collect",
                "--unit=agentcompute-capture-smoke", EDITOR_LAUNCH,
            ],
            user=True,
        )
        if launched.returncode != 0:
            die(f"launching {EDITOR_LAUNCH} failed: {launched.stderr}")
        wait_until(
            min(deadline, time.time() + 30),
            "whole-desktop pixels to change after launching Text Editor",
            lambda: capture("desktop.png") != before,
        )
        note("whole-desktop pixels changed after launching Text Editor", log)

        result = {
            "fingerprint": fingerprint,
            "metadata": str(metadata),
            "disk": str(disk),
            "remote": remote,
            "project": project,
            "profile": profile,
            "instance": name,
            "x11": True,
            "cua_driver_unit": "active",
            "list_apps": len(apps),
            "desktop_capture_live": True,
            "security_nesting": "false",
        }
        (evidence / "result.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(result))
    except Error as exc:
        note(str(exc), log)
        exit_code = 1
    finally:
        try:
            cleanup()
        except Error as exc:
            note(str(exc), log)
            exit_code = 1
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
