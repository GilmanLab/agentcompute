#!/usr/bin/env python3
"""Boot-test a split runner VM against the incus-gh-runner v2.0.0 guest contract.

Guide step 10: path unit armed, growroot, bogus payload lifecycle
(status/console/payload deletion/poweroff), no GitHub credentials.

Usage:
  python3 images/runner/smoke.py \\
    --metadata PATH/incus.tar.xz --disk PATH/disk.qcow2 \\
    --remote nas01 --project image-build --profile runner-smoke \\
    --evidence DIR
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

BOGUS_JIT = "bogus-not-a-github-jit-config"
GROW_MIN_BYTES = 10 * 1024 * 1024 * 1024


class Error(RuntimeError):
    """Smoke failed."""


def note(message: str, log: Path) -> None:
    line = f"runner-smoke: {message}"
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


def instance_status(remote: str, project: str, name: str) -> str:
    result = run(
        ["incus", "query", f"{remote}:/1.0/instances/{name}?project={project}"],
        capture_output=True,
    )
    if result.returncode != 0:
        return ""
    try:
        return str(json.loads(result.stdout).get("status") or "")
    except json.JSONDecodeError:
        return ""


def pull_status(remote: str, project: str, name: str, dest: Path) -> dict[str, object] | None:
    dest.unlink(missing_ok=True)
    result = incus(
        project,
        ["file", "pull", f"{remote}:{name}/run/incus-gh-runner/status.json", str(dest)],
        capture_output=True,
    )
    if result.returncode != 0 or not dest.is_file():
        return None
    try:
        data = json.loads(dest.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    return data if isinstance(data, dict) else None


def watch_status(remote: str, project: str, name: str, evidence: Path) -> subprocess.Popen[str]:
    with tempfile.TemporaryDirectory(prefix="runner-status-observer-") as temporary:
        binary = Path(temporary) / "observe-status"
        source = Path(__file__).with_name("observe_status_linux.go")
        compiled = run(
            ["go", "build", "-o", str(binary), str(source)],
            env={**os.environ, "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"},
            capture_output=True,
        )
        if compiled.returncode:
            die(f"status observer build failed: {compiled.stderr}")
        pushed = incus(project, ["file", "push", "--mode=0700", "--uid=0", "--gid=0",
                                 str(binary), f"{remote}:{name}/usr/local/libexec/observe-status"],
                       capture_output=True)
        if pushed.returncode:
            die(f"status observer push failed: {pushed.stderr}")
    with (evidence / "status-transitions.jsonl").open("w", encoding="utf-8") as output:
        return subprocess.Popen(
            ["incus", "--quiet", "--project", project, "exec", f"{remote}:{name}",
             "--", "/usr/local/libexec/observe-status"],
            text=True, stdout=output, stderr=subprocess.STDOUT,
        )


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--metadata", type=Path, required=True)
    parser.add_argument("--disk", type=Path, required=True)
    parser.add_argument("--remote", required=True)
    parser.add_argument("--project", required=True)
    parser.add_argument("--profile", required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--timeout", type=int, default=420)
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
    if shutil.which("go") is None:
        die("Go compiler required for the disposable guest status observer")

    evidence.mkdir(parents=True, exist_ok=True)
    log = evidence / "smoke.log"
    log.write_text("", encoding="utf-8")
    suffix = time.strftime("%Y%m%d%H%M%S", time.gmtime()) + f"-{os.getpid()}"
    name = f"runner-smoke-{suffix}"
    if len(name) > 63:
        die(f"derived name is longer than 63 characters: {name}")

    imported = False
    created = False
    fingerprint = ""
    watcher: subprocess.Popen[str] | None = None

    def cleanup() -> None:
        failures = 0
        if watcher is not None and watcher.poll() is None:
            watcher.terminate()
            try:
                watcher.wait(timeout=5)
            except subprocess.TimeoutExpired:
                watcher.kill()
                watcher.wait()
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
            imported_run = incus(project, ["image", "import", str(metadata), str(disk), f"{remote}:"],
                                 capture_output=True)
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
            ["init", f"{remote}:{fingerprint}", f"{remote}:{name}", "--vm", "--profile", profile],
            capture_output=True,
        )
        if init.returncode != 0:
            die(f"init failed: {(init.stderr or init.stdout or '').strip()}")
        created = True
        start = incus(project, ["start", f"{remote}:{name}"], capture_output=True)
        if start.returncode != 0:
            die(f"start failed: {(start.stderr or start.stdout or '').strip()}")

        deadline = time.time() + timeout
        note("waiting for incus-agent", log)

        def agent_up() -> bool:
            return incus(project, ["exec", f"{remote}:{name}", "--", "/bin/true"], capture_output=True).returncode == 0

        wait_until(deadline, "incus-agent", agent_up)

        def path_ready() -> bool:
            path_unit = incus(
                project,
                ["exec", f"{remote}:{name}", "--", "systemctl", "is-active", "incus-gh-runner-guest.path"],
                capture_output=True,
            )
            return (path_unit.stdout or "").strip() == "active"

        wait_until(deadline, "incus-gh-runner-guest.path", path_ready)
        note("path unit is active", log)

        runner = incus(
            project,
            ["exec", f"{remote}:{name}", "--", "runuser", "-u", "actions-runner", "--",
             "/opt/actions-runner/bin/Runner.Listener", "--version"],
            capture_output=True,
        )
        runner_version = (runner.stdout or "").strip()
        if runner.returncode != 0 or not re.fullmatch(r"\d+\.\d+\.\d+", runner_version):
            die(f"Actions Runner cannot execute as actions-runner: {runner.stdout}{runner.stderr}")
        note(f"Actions Runner {runner_version} executes as actions-runner", log)

        def network_ready() -> bool:
            routes = incus(
                project, ["exec", f"{remote}:{name}", "--", "ip", "-j", "route", "show", "default"],
                capture_output=True,
            )
            return routes.returncode == 0 and any(route.get("gateway") for route in json.loads(routes.stdout))

        wait_until(deadline, "guest DHCP default route", network_ready)
        note("guest DHCP default route is configured", log)

        df = incus(
            project,
            ["exec", f"{remote}:{name}", "--", "df", "-B1", "--output=size", "/"],
            capture_output=True,
        )
        if df.returncode != 0:
            die("df / failed")
        numbers = [int(tok) for tok in (df.stdout or "").split() if tok.isdigit()]
        if not numbers:
            die(f"could not parse df / output: {df.stdout!r}")
        root_bytes = numbers[-1]
        (evidence / "growroot.txt").write_text(f"{root_bytes}\n", encoding="utf-8")
        if root_bytes < GROW_MIN_BYTES:
            die(f"root filesystem is {root_bytes} bytes; growroot/growfs did not reach {GROW_MIN_BYTES}")
        note(f"root filesystem is {root_bytes} bytes after growroot", log)

        watcher = watch_status(remote, project, name, evidence)
        transitions = evidence / "status-transitions.jsonl"

        def observer_ready() -> bool:
            if watcher.poll() is not None:
                die(f"status observer exited before payload: {transitions.read_text()}")
            return '"observer":"ready"' in transitions.read_text()

        wait_until(min(deadline, time.time() + 20), "status observer", observer_ready)

        payload = {"version": 1, "jit_config": BOGUS_JIT}
        with tempfile.NamedTemporaryFile("w", encoding="utf-8", suffix=".json", delete=False) as handle:
            json.dump(payload, handle)
            payload_path = Path(handle.name)
        ready_fd, ready_name = tempfile.mkstemp()
        os.close(ready_fd)
        ready_path = Path(ready_name)
        try:
            pushed = incus(
                project,
                [
                    "file",
                    "push",
                    "--mode=0600",
                    "--uid=0",
                    "--gid=0",
                    str(payload_path),
                    f"{remote}:{name}/run/incus-gh-runner/payload.json",
                ],
                capture_output=True,
            )
            if pushed.returncode != 0:
                die(f"payload.json push failed: {(pushed.stderr or pushed.stdout or '').strip()}")
            ready = incus(
                project,
                [
                    "file",
                    "push",
                    "--mode=0600",
                    "--uid=0",
                    "--gid=0",
                    str(ready_path),
                    f"{remote}:{name}/run/incus-gh-runner/payload.ready.tmp",
                ],
                capture_output=True,
            )
            if ready.returncode != 0:
                die(f"payload.ready push failed: {(ready.stderr or ready.stdout or '').strip()}")
            # Publish the marker atomically: the guest immediately unlinks it.
            ready = incus(
                project,
                ["exec", f"{remote}:{name}", "--", "mv",
                 "/run/incus-gh-runner/payload.ready.tmp", "/run/incus-gh-runner/payload.ready"],
                capture_output=True,
            )
            if ready.returncode != 0:
                die(f"payload.ready rename failed: {(ready.stderr or ready.stdout or '').strip()}")
        finally:
            payload_path.unlink(missing_ok=True)
            ready_path.unlink(missing_ok=True)
        note("wrote bogus payload (no GitHub credentials)", log)

        seen: list[str] = []
        status_dest = evidence / "status.json"
        payload_deleted = False
        console_text = ""
        while time.time() < deadline:
            status = pull_status(remote, project, name, status_dest)
            if status:
                state = str(status.get("state") or "")
                if state and (not seen or seen[-1] != state):
                    seen.append(state)
                    note(f"status {state}", log)
                if state in {"running", "exited", "failed"} and not payload_deleted:
                    missing_json = incus(
                        project,
                        ["exec", f"{remote}:{name}", "--", "test", "!", "-e", "/run/incus-gh-runner/payload.json"],
                        capture_output=True,
                    )
                    missing_ready = incus(
                        project,
                        ["exec", f"{remote}:{name}", "--", "test", "!", "-e", "/run/incus-gh-runner/payload.ready"],
                        capture_output=True,
                    )
                    if missing_json.returncode != 0 or missing_ready.returncode != 0:
                        die("payload.json/payload.ready still present after runner start")
                    payload_deleted = True
                    note("payload files deleted before runner start", log)
                if state in {"exited", "failed"} and "incus-gh-runner-guest action=poweroff" not in console_text:
                    console = incus(project, ["console", f"{remote}:{name}", "--show-log"], capture_output=True)
                    if console.returncode != 0:
                        die(f"serial console read failed: {console.stderr}")
                    console_text += console.stdout or ""
                    (evidence / "console.log").write_text(console_text, encoding="utf-8")
            current = instance_status(remote, project, name)
            if current.lower() == "stopped":
                note("instance powered off", log)
                break
            time.sleep(2)
        else:
            die("instance did not power off before timeout")

        try:
            if watcher.wait(timeout=10) != 0:
                die(f"status observer failed: {transitions.read_text()}")
        except subprocess.TimeoutExpired:
            die("status observer did not finish before guest poweroff")
        try:
            seen = [record["state"] for line in transitions.read_text().splitlines()
                    if "state" in (record := json.loads(line))]
        except (ValueError, KeyError) as exc:
            die(f"invalid status transition evidence: {exc}")
        if not payload_deleted:
            die("never observed payload deletion")
        if seen[:2] != ["starting", "running"]:
            die(f"status sequence {seen} did not start starting -> running")
        if "exited" not in seen and "failed" not in seen:
            die("status observer did not record the terminal state")

        required = [
            "incus-gh-runner-guest state=starting",
            "incus-gh-runner-guest state=running",
            "incus-gh-runner-guest state=exited",
            "incus-gh-runner-guest action=poweroff",
        ]
        missing = [line for line in required if line not in console_text]
        if missing:
            die("serial console missing " + ", ".join(missing))
        if BOGUS_JIT in console_text:
            die("serial console leaked the bogus jit_config")
        note("console lifecycle lines present; no jit_config leak", log)

        result = {
            "fingerprint": fingerprint,
            "metadata": str(metadata),
            "disk": str(disk),
            "remote": remote,
            "project": project,
            "profile": profile,
            "instance": name,
            "root_bytes": root_bytes,
            "runner_version": runner_version,
            "states": seen,
            "payload_deleted": payload_deleted,
            "powered_off": True,
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
