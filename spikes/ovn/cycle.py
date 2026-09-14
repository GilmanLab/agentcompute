#!/usr/bin/env python3
"""One-command OVN create/probe/destroy cycle with bounded diagnostics.

Invoked as: spikes/ovn/probe.sh cycle --evidence-dir PATH [--keep-on-failure]

Reuses topology.py for lifecycle. Does not stop central, run keeper, or
clear neighbor/ARP state. One create attempt; no bootstrap retries.
"""

import argparse
import json
import os
import shutil
import signal
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlencode

HERE = Path(__file__).resolve().parent
TOPOLOGY = HERE / "topology.py"
PROBE = HERE / "probe.sh"

REMOTE = "nas01"
PROJECT = "ac-ovn-spike"
NETWORK = "default"
INSTANCE_ONE = "one"
INSTANCE_TWO = "two"
FORWARD_LISTEN = "10.10.40.79"
FORWARD_PORT = 8080
MARKER = "ovn-spike-ok"
UPLINK_GATEWAY = "10.10.40.1"
DNS_NAME = "example.com"

GATEWAY_KEY = Path("/Users/josh/.ssh/vyos-gateway")
SSH_SANDBOX = [
    "ssh",
    "-o",
    "BatchMode=yes",
    "-o",
    "ConnectTimeout=10",
    "josh@sandbox01",
]
SSH_GATEWAY = [
    "ssh",
    "-o",
    "BatchMode=yes",
    "-o",
    "ConnectTimeout=10",
    "-i",
    str(GATEWAY_KEY),
    "vyos@10.0.0.2",
]

CREATE_TIMEOUT = 900
DESTROY_TIMEOUT = 300
PROBE_TIMEOUT = 600
DIAG_TIMEOUT = 45
TCPDUMP_READY_TIMEOUT = 15
MONITOR_READY_TIMEOUT = 15
TCPDUMP_BOUND = 180
WEBSOCKET_READY = "Connected to the websocket:"
EVENTS_PATH = "/1.0/events"


def utcnow():
    return datetime.now(timezone.utc).isoformat()


def fail_preflight(message):
    print(f"cycle: {message}", file=sys.stderr)
    raise SystemExit(2)


class Capture:
    """Background process with a stdout log and optional client-debug stderr."""

    def __init__(self, name, args, log_path):
        self.name = name
        self.args = args
        self.log_path = Path(log_path)
        self.debug_path = None
        self.proc = None
        self.log = None
        self.pid = None
        self.started_at = None
        self.stopped_at = None
        self.returncode = None
        self.ready = False
        self.ready_line = None
        self.error = None
        self.thread = None
        self.ready_event = threading.Event()

    def alive(self):
        return self.proc is not None and self.proc.poll() is None


class Cycle:
    def __init__(self, evidence_dir, keep_on_failure):
        self.evidence = Path(evidence_dir)
        self.keep_on_failure = keep_on_failure
        self.commands_path = self.evidence / "commands.jsonl"
        self.topology_log = self.evidence / "topology.jsonl"
        self.started_at = utcnow()
        self.t0 = time.monotonic()
        self.captures = []
        self.check_failures = []
        self.capture_failures = []
        self.cleanup_failures = []
        self.external_ip = None
        self.router_mac = None
        self.create = None
        self.probe = None
        self.fallback = None
        self.destroy = None
        self.teardown = "not-attempted"
        self.create_attempted = False

    def record(self, event):
        event.setdefault("at", utcnow())
        with self.commands_path.open("a", encoding="utf-8") as fh:
            fh.write(json.dumps(event, default=str) + "\n")

    def run(self, args, timeout=180, cwd=None):
        started = time.monotonic()
        try:
            result = subprocess.run(
                args,
                stdin=subprocess.DEVNULL,
                capture_output=True,
                text=True,
                timeout=timeout,
                cwd=cwd,
            )
        except subprocess.TimeoutExpired as exc:
            stdout = (
                exc.stdout
                if isinstance(exc.stdout, str)
                else (exc.stdout or b"").decode("utf-8", "replace")
            )
            stderr = (
                exc.stderr
                if isinstance(exc.stderr, str)
                else (exc.stderr or b"").decode("utf-8", "replace")
            )
            event = {
                "command": args,
                "seconds": round(time.monotonic() - started, 3),
                "exit_code": None,
                "error": "timeout",
                "stdout": stdout,
                "stderr": stderr,
            }
            self.record(event)
            return subprocess.CompletedProcess(args, 124, stdout, stderr)
        event = {
            "command": args,
            "seconds": round(time.monotonic() - started, 3),
            "exit_code": result.returncode,
            "stdout": result.stdout,
            "stderr": result.stderr,
            "error": None
            if result.returncode == 0
            else (
                result.stderr.strip()
                or result.stdout.strip()
                or f"exit {result.returncode}"
            ),
        }
        self.record(event)
        return result

    def _pump_monitor_stderr(self, cap):
        kept = []
        try:
            for raw in iter(cap.proc.stderr.readline, b""):
                line = raw.decode("utf-8", "replace")
                if "BEGIN CERTIFICATE" in line or "BEGIN PRIVATE" in line:
                    continue
                if (
                    WEBSOCKET_READY in line
                    and EVENTS_PATH in line
                    and "all-projects=true" in line
                ):
                    kept.append(line)
                    if cap.ready_line is None or "all-projects" in line:
                        cap.ready_line = line.strip()
                    cap.ready_event.set()
                elif "error" in line.lower() or "fail" in line.lower():
                    kept.append(line)
        finally:
            if cap.debug_path is not None:
                cap.debug_path.write_text("".join(kept), encoding="utf-8")

    def start_capture(
        self,
        name,
        args,
        log_name,
        ready_needle=None,
        ready_timeout=TCPDUMP_READY_TIMEOUT,
        debug_log_name=None,
        websocket_ready=False,
    ):
        path = self.evidence / log_name
        cap = Capture(name, args, path)
        cap.log = path.open("wb")
        cap.started_at = utcnow()
        popen_kwargs = {
            "stdin": subprocess.DEVNULL,
            "stdout": cap.log,
            "start_new_session": True,
        }
        if websocket_ready:
            cap.debug_path = self.evidence / debug_log_name
            popen_kwargs["stderr"] = subprocess.PIPE
        else:
            popen_kwargs["stderr"] = subprocess.STDOUT
        try:
            cap.proc = subprocess.Popen(args, **popen_kwargs)
        except OSError as exc:
            cap.log.close()
            cap.error = str(exc)
            self.record(
                {"command": args, "error": str(exc), "capture": name, "ready": False}
            )
            self.capture_failures.append(f"{name}: failed to spawn: {exc}")
            self.captures.append(cap)
            return cap
        cap.pid = cap.proc.pid
        self.captures.append(cap)
        if websocket_ready:
            cap.thread = threading.Thread(
                target=self._pump_monitor_stderr, args=(cap,), daemon=True
            )
            cap.thread.start()
            deadline = time.monotonic() + ready_timeout
            while time.monotonic() < deadline:
                if cap.ready_event.wait(timeout=0.05):
                    cap.ready = True
                    self.record(
                        {
                            "command": args,
                            "capture": name,
                            "pid": cap.pid,
                            "ready": True,
                            "ready_signal": cap.ready_line,
                        }
                    )
                    return cap
                if cap.proc.poll() is not None:
                    break
            cap.error = f"no {WEBSOCKET_READY!r} {EVENTS_PATH} on client debug stderr within {ready_timeout}s"
            if cap.proc.poll() is not None:
                cap.returncode = cap.proc.returncode
                cap.error = f"exited {cap.returncode} before websocket ready"
            self.record(
                {
                    "command": args,
                    "capture": name,
                    "pid": cap.pid,
                    "ready": False,
                    "error": cap.error,
                }
            )
            self.capture_failures.append(f"{name}: {cap.error}")
            return cap

        deadline = time.monotonic() + ready_timeout
        while time.monotonic() < deadline:
            if cap.proc.poll() is not None:
                break
            cap.log.flush()
            try:
                text = path.read_text(errors="replace")
            except OSError:
                text = ""
            if ready_needle and ready_needle in text:
                cap.ready = True
                cap.ready_line = ready_needle
                self.record(
                    {
                        "command": args,
                        "capture": name,
                        "pid": cap.pid,
                        "ready": True,
                        "ready_signal": ready_needle,
                    }
                )
                return cap
            time.sleep(0.05)
        cap.error = f"not ready ({ready_needle!r}) within {ready_timeout}s"
        if cap.proc.poll() is not None:
            cap.returncode = cap.proc.returncode
            cap.error = f"exited {cap.returncode} before {ready_needle!r}"
        self.record(
            {
                "command": args,
                "capture": name,
                "pid": cap.pid,
                "ready": False,
                "error": cap.error,
            }
        )
        self.capture_failures.append(f"{name}: {cap.error}")
        return cap

    def stop_capture(self, cap, timeout=5):
        if cap is None or cap.proc is None:
            return
        if cap.proc.poll() is None:
            try:
                os.killpg(cap.proc.pid, signal.SIGTERM)
            except (ProcessLookupError, PermissionError, OSError):
                cap.proc.terminate()
            try:
                cap.proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(cap.proc.pid, signal.SIGKILL)
                except (ProcessLookupError, PermissionError, OSError):
                    cap.proc.kill()
                cap.proc.wait(timeout=timeout)
        cap.returncode = cap.proc.returncode
        cap.stopped_at = utcnow()
        if cap.thread is not None:
            cap.thread.join(timeout=2)
            cap.thread = None
        if cap.log is not None:
            try:
                cap.log.flush()
                cap.log.close()
            except OSError:
                pass
            cap.log = None
        self.record(
            {
                "capture": cap.name,
                "pid": cap.pid,
                "exit_code": cap.returncode,
                "stopped": True,
                "log": str(cap.log_path),
                "ready_signal": cap.ready_line,
            }
        )

    def stop_all(self):
        for cap in list(self.captures):
            if cap.proc is not None and cap.log is not None:
                self.stop_capture(cap)

    def parse_json(self, text):
        text = (text or "").strip()
        if not text:
            return None
        try:
            return json.loads(text)
        except json.JSONDecodeError:
            return None

    def router_external_ipv4(self, network):
        config = (network or {}).get("config") or {}
        for key in ("ipv4.nat.address", "volatile.network.ipv4.address"):
            value = config.get(key)
            if value:
                return value
        ovn = (network or {}).get("ovn") or {}
        return ovn.get("uplink_ipv4") or ""

    def query_network(self):
        result = self.run(
            ["incus", "query", f"{REMOTE}:/1.0/networks/{NETWORK}?project={PROJECT}"],
            timeout=DIAG_TIMEOUT,
        )
        if result.returncode != 0:
            return None
        return self.parse_json(result.stdout)

    def query_instances(self):
        result = self.run(
            [
                "incus",
                "query",
                f"{REMOTE}:/1.0/instances?project={PROJECT}&recursion=1",
            ],
            timeout=DIAG_TIMEOUT,
        )
        if result.returncode != 0:
            return []
        body = self.parse_json(result.stdout)
        return body if isinstance(body, list) else []

    def running_guests(self):
        names = []
        for inst in self.query_instances():
            name = inst.get("name")
            status = inst.get("status")
            if name in (INSTANCE_ONE, INSTANCE_TWO) and status == "Running":
                names.append(name)
        return names

    def write_text(self, name, text):
        path = self.evidence / name
        path.write_text(text if text is not None else "", encoding="utf-8")
        return path

    def collect_diagnostics(self, label, external_ip):
        path = self.evidence / f"diagnostics-{label}.txt"
        chunks = []
        mac = None

        def add(title, result):
            chunks.append(f"=== {title} exit={result.returncode} ===\n")
            if result.stdout:
                chunks.append(result.stdout)
                if not result.stdout.endswith("\n"):
                    chunks.append("\n")
            if result.stderr:
                chunks.append(result.stderr)
                if not result.stderr.endswith("\n"):
                    chunks.append("\n")
            if result.returncode and not (
                label == "after-teardown" and title.startswith("incus network")
            ):
                self.capture_failures.append(
                    f"{label}: {title} exited {result.returncode}"
                )

        show = self.run(
            ["incus", "network", "show", f"{REMOTE}:{NETWORK}", "--project", PROJECT],
            timeout=DIAG_TIMEOUT,
        )
        add("incus network show", show)

        alloc = self.run(
            [
                "incus",
                "network",
                "list-allocations",
                f"{REMOTE}:",
                "--project",
                PROJECT,
            ],
            timeout=DIAG_TIMEOUT,
        )
        add("incus network list-allocations", alloc)
        if external_ip and alloc.stdout:
            for line in alloc.stdout.splitlines():
                if external_ip in line:
                    parts = [p.strip() for p in line.strip("|").split("|")]
                    if len(parts) >= 5:
                        candidate = parts[-1]
                        if ":" in candidate and candidate.lower() != "mac address":
                            mac = candidate

        ip = external_ip or ""
        sandbox_script = "\n".join(
            [
                "set -e",
                "echo '=== ovn-nbctl show ==='",
                "sudo -n ovn-nbctl show",
                "echo '=== ovn-sbctl show ==='",
                "sudo -n ovn-sbctl show",
                "echo '=== ovn-nbctl list Logical_Router_Port ==='",
                "sudo -n ovn-nbctl list Logical_Router_Port",
                "echo '=== ovn-sbctl find Port_Binding type=chassisredirect ==='",
                "sudo -n ovn-sbctl find Port_Binding type=chassisredirect",
                "echo '=== ovn-nbctl list HA_Chassis_Group ==='",
                "sudo -n ovn-nbctl list HA_Chassis_Group",
                "echo '=== ovn-nbctl list HA_Chassis ==='",
                "sudo -n ovn-nbctl list HA_Chassis",
                "echo '=== ovn-sbctl list Chassis ==='",
                "sudo -n ovn-sbctl --format=json --columns=name,hostname,other_config list Chassis",
                "echo '=== ip neigh ==='",
                f"if [ -n '{ip}' ]; then ip neigh show {ip}; else ip neigh; fi",
            ]
        )
        sandbox = self.run(SSH_SANDBOX + [sandbox_script], timeout=DIAG_TIMEOUT)
        add("sandbox01 nb/sb/neigh", sandbox)
        if mac is None and sandbox.stdout and ip:
            current_mac = None
            for raw in sandbox.stdout.splitlines():
                line = raw.strip()
                if not line:
                    current_mac = None
                if line.startswith("mac "):
                    current_mac = line.split(":", 1)[-1].strip().strip('"')
                if ip in line and current_mac:
                    mac = current_mac

        if ip:
            gw = self.run(SSH_GATEWAY + [f"ip neigh show {ip}"], timeout=DIAG_TIMEOUT)
        else:
            gw = self.run(SSH_GATEWAY + ["ip neigh"], timeout=DIAG_TIMEOUT)
        add("gateway neigh", gw)

        for member in ("lab01", "lab02", "lab03", "nas01"):
            params = urlencode(
                {
                    "target": member,
                    "unit": "ovs-vswitchd.service",
                    "since": self.started_at,
                }
            )
            journal = self.run(
                [
                    "bash",
                    "-o",
                    "pipefail",
                    "-c",
                    f"incus query '{REMOTE}:/os/1.0/debug/log?{params}'"
                    " | jq '[.[] | {at: .__REALTIME_TIMESTAMP, member: ._HOSTNAME, message: .MESSAGE}]'",
                ],
                timeout=DIAG_TIMEOUT,
            )
            add(f"{member} ovs-vswitchd journal since cycle start", journal)

        if mac:
            mac = mac.strip('[]"').split()[0]
        path.write_text("".join(chunks), encoding="utf-8")
        self.record({"diagnostics": label, "path": str(path), "router_mac": mac})
        if mac and not self.router_mac:
            self.router_mac = mac
        return path

    def guest_exec(self, name, command):
        return self.run(
            [
                "incus",
                "exec",
                f"{REMOTE}:{name}",
                "--project",
                PROJECT,
                "--",
                "sh",
                "-c",
                command,
            ],
            timeout=DIAG_TIMEOUT,
        )

    def run_fallback(self, guests):
        rows = []
        result = self.run(
            ["incus", "list", f"{REMOTE}:", "--project", PROJECT, "--format", "json"]
        )
        addresses = {}
        for instance in self.parse_json(result.stdout) or []:
            for address in (
                instance.get("state", {})
                .get("network", {})
                .get("eth0", {})
                .get("addresses", [])
            ):
                if address["family"] == "inet" and address["scope"] == "global":
                    addresses[instance["name"]] = address["address"]
        for name in guests:
            other = INSTANCE_TWO if name == INSTANCE_ONE else INSTANCE_ONE
            if other not in addresses:
                rows.append(
                    {
                        "guest": name,
                        "check": "cross-member-ping",
                        "exit_code": None,
                        "error": "peer has no global IPv4",
                    }
                )
            for title, command in (
                *(
                    [("cross-member-ping", f"ping -c 3 -W 2 {addresses[other]}")]
                    if other in addresses
                    else []
                ),
                ("gateway-ping", f"ping -c 3 -W 2 {UPLINK_GATEWAY}"),
                ("dns", f"nslookup {DNS_NAME} {UPLINK_GATEWAY}"),
                ("wget", f"wget -T 15 -qO /dev/null https://{DNS_NAME}"),
            ):
                result = self.guest_exec(name, command)
                rows.append(
                    {
                        "guest": name,
                        "check": title,
                        "command": command,
                        "exit_code": result.returncode,
                        "stdout": result.stdout,
                        "stderr": result.stderr,
                    }
                )
        self.write_text("fallback-checks.json", json.dumps(rows, indent=2) + "\n")
        return rows

    def capture_summary(self):
        rows = []
        for cap in self.captures:
            size = 0
            if cap.log_path.exists():
                size = cap.log_path.stat().st_size
            rows.append(
                {
                    "name": cap.name,
                    "ready": cap.ready,
                    "pid": cap.pid,
                    "returncode": cap.returncode,
                    "error": cap.error,
                    "log": str(cap.log_path),
                    "bytes": size,
                    "ready_signal": cap.ready_line,
                    "started_at": cap.started_at,
                    "stopped_at": cap.stopped_at,
                }
            )
        return rows

    def summary(self, exit_code):
        datapath_ok = (
            bool(
                self.create
                and self.create.get("ok")
                and self.probe
                and self.probe.get("ok")
            )
            and not self.check_failures
        )
        captures_ok = not self.capture_failures
        cleanup_ok = not self.cleanup_failures
        return {
            "ok": datapath_ok and captures_ok and cleanup_ok,
            "datapath_ok": datapath_ok,
            "captures_ok": captures_ok,
            "cleanup_ok": cleanup_ok,
            "exit_code": exit_code,
            "keep_on_failure": self.keep_on_failure,
            "teardown": self.teardown,
            "started_at": self.started_at,
            "ended_at": utcnow(),
            "seconds": round(time.monotonic() - self.t0, 3),
            "external_ip": self.external_ip,
            "router_mac": self.router_mac,
            "create": self.create,
            "probe": self.probe,
            "fallback_checks": self.fallback,
            "destroy": self.destroy,
            "captures": self.capture_summary(),
            "failures": {
                "checks": self.check_failures,
                "captures": self.capture_failures,
                "cleanup": self.cleanup_failures,
            },
            "evidence_dir": str(self.evidence),
        }

    def write_summary(self, body):
        path = self.evidence / "cycle.json"
        path.write_text(
            json.dumps(body, indent=2, default=str) + "\n", encoding="utf-8"
        )
        return path

    def monitor_args(self):
        return [
            "incus",
            "--debug",
            "monitor",
            f"{REMOTE}:",
            "--type=logging",
            "--pretty",
            "--loglevel=debug",
            "--all-projects",
        ]

    def start_monitor(self, name, log_name, debug_name):
        return self.start_capture(
            name,
            self.monitor_args(),
            log_name,
            websocket_ready=True,
            debug_log_name=debug_name,
            ready_timeout=MONITOR_READY_TIMEOUT,
        )

    def note_capture_failure(self, message):
        if message not in self.capture_failures:
            self.capture_failures.append(message)

    def require_capture_log(self, name, required):
        if not required:
            return
        cap = next((c for c in self.captures if c.name == name), None)
        if cap is None:
            self.note_capture_failure(f"{name}: not started")
            return
        if not cap.ready:
            self.note_capture_failure(f"{name}: not ready")
            return
        if not cap.log_path.exists() or cap.log_path.stat().st_size == 0:
            self.note_capture_failure(f"{name}: empty log")

    def run_create(self):
        monitor = self.start_monitor(
            "monitor-create", "monitor-create.log", "monitor-create.debug.log"
        )
        if not monitor.ready:
            raise RuntimeError("create monitor did not establish its event websocket")
        self.create_attempted = True
        result = self.run(
            [sys.executable, str(TOPOLOGY), "--log", str(self.topology_log), "create"],
            timeout=CREATE_TIMEOUT,
        )
        self.stop_capture(monitor)
        self.write_text("create.stdout.json", result.stdout)
        self.write_text("create.stderr.txt", result.stderr)
        body = self.parse_json(result.stdout) or {}
        self.create = {
            "ok": result.returncode == 0,
            "exit_code": result.returncode,
            "result": body,
            "stderr": result.stderr,
        }
        if result.returncode != 0:
            error = (
                body.get("error")
                or result.stderr.strip()
                or f"exit {result.returncode}"
            )
            if "refusing to adopt existing project" in error:
                self.create_attempted = False
            self.check_failures.append(f"create: {error}")
            if "apk" in error.lower():
                self.create["note"] = (
                    "setup failed during guest package install; "
                    "fallback gateway/DNS/wget collected; "
                    "not classified as a package-name error"
                )
        else:
            self.external_ip = body.get("external_ip") or self.external_ip
        if not self.external_ip:
            network = self.query_network()
            self.external_ip = self.router_external_ipv4(network) or None
        self.require_capture_log("monitor-create", required=True)
        if monitor.ready and (result.returncode == 0 or self.external_ip):
            text = ""
            if monitor.log_path.exists():
                text = monitor.log_path.read_text(errors="replace")
            if not text.strip():
                self.note_capture_failure(
                    "monitor-create: no logging events captured during create"
                )

    def run_probe_or_fallback(self):
        guests = self.running_guests()
        want_tcpdump = bool(self.external_ip) and bool(guests)
        tcpdump = None
        if want_tcpdump:
            remote = (
                f"sudo -n timeout {TCPDUMP_BOUND} tcpdump -nne -l -i enp2s0 "
                f"arp and host {self.external_ip}"
            )
            tcpdump = self.start_capture(
                "tcpdump-arp",
                SSH_SANDBOX[:1] + ["-tt"] + SSH_SANDBOX[1:] + [remote],
                "tcpdump-arp.log",
                ready_needle="listening on",
                ready_timeout=TCPDUMP_READY_TIMEOUT,
            )
        elif guests and not self.external_ip:
            self.note_capture_failure("tcpdump-arp: no allocated router IP to filter")

        if self.create and self.create.get("ok"):
            url = f"http://{FORWARD_LISTEN}:{FORWARD_PORT}/index.html"
            result = self.run(
                [
                    "bash",
                    str(PROBE),
                    REMOTE,
                    PROJECT,
                    NETWORK,
                    INSTANCE_ONE,
                    INSTANCE_TWO,
                    url,
                    MARKER,
                ],
                timeout=PROBE_TIMEOUT,
            )
            self.write_text("probe.stdout.txt", result.stdout)
            self.write_text("probe.stderr.txt", result.stderr)
            self.probe = {
                "ok": result.returncode == 0,
                "exit_code": result.returncode,
                "stdout": result.stdout,
                "stderr": result.stderr,
            }
            if result.returncode != 0:
                detail = (
                    result.stderr.strip()
                    or result.stdout.strip()
                    or f"exit {result.returncode}"
                )
                self.check_failures.append(f"probe: {detail}")
        else:
            if guests:
                self.fallback = self.run_fallback(guests)
            else:
                self.fallback = []
                if self.create_attempted and not (
                    self.create and self.create.get("ok")
                ):
                    self.record({"fallback": "skipped", "reason": "no running guests"})

        if tcpdump is not None:
            self.stop_capture(tcpdump)
            self.require_capture_log("tcpdump-arp", required=True)
        elif want_tcpdump:
            self.require_capture_log("tcpdump-arp", required=True)

    def run_destroy(self):
        failed = bool(self.check_failures or self.capture_failures)
        if failed and self.keep_on_failure:
            self.teardown = "skipped"
            self.destroy = {"ok": True, "skipped": True, "reason": "keep-on-failure"}
            return
        monitor = self.start_monitor(
            "monitor-delete", "monitor-delete.log", "monitor-delete.debug.log"
        )
        result = self.run(
            [sys.executable, str(TOPOLOGY), "--log", str(self.topology_log), "destroy"],
            timeout=DESTROY_TIMEOUT,
        )
        self.stop_capture(monitor)
        self.write_text("destroy.stdout.json", result.stdout)
        self.write_text("destroy.stderr.txt", result.stderr)
        body = self.parse_json(result.stdout) or {}
        self.destroy = {
            "ok": result.returncode == 0,
            "skipped": False,
            "exit_code": result.returncode,
            "result": body,
            "stderr": result.stderr,
        }
        if result.returncode != 0:
            error = (
                body.get("error")
                or result.stderr.strip()
                or f"exit {result.returncode}"
            )
            failures = body.get("failures") or []
            if failures:
                error = "; ".join(str(item) for item in failures)
            self.cleanup_failures.append(f"destroy: {error}")
            self.teardown = "failed"
        else:
            self.teardown = "done"
        self.require_capture_log("monitor-delete", required=True)

    def execute(self):
        try:
            self.run_create()
            self.collect_diagnostics("before-probe", self.external_ip)
            self.run_probe_or_fallback()
            self.collect_diagnostics("after-probe", self.external_ip)
        except (Exception, KeyboardInterrupt) as exc:
            self.record({"cycle_error": str(exc) or type(exc).__name__})
            if not self.capture_failures:
                self.check_failures.append(str(exc) or type(exc).__name__)
        finally:
            try:
                if self.create_attempted:
                    self.run_destroy()
                    if self.teardown in ("done", "failed"):
                        self.collect_diagnostics("after-teardown", self.external_ip)
            except (Exception, KeyboardInterrupt) as exc:
                self.cleanup_failures.append(str(exc) or type(exc).__name__)
                self.teardown = "failed"
            finally:
                self.stop_all()
        exit_code = (
            0
            if not (
                self.check_failures or self.capture_failures or self.cleanup_failures
            )
            else 1
        )
        body = self.summary(exit_code)
        self.write_summary(body)
        json.dump(body, sys.stdout, indent=2, default=str)
        sys.stdout.write("\n")
        raise SystemExit(exit_code)


def validate_evidence_dir(raw):
    path = Path(raw).expanduser()
    if not path.is_absolute():
        path = Path.cwd() / path
    if path.exists() and not path.is_dir():
        fail_preflight(f"evidence-dir is not a directory: {path}")
    if path.exists() and any(path.iterdir()):
        fail_preflight(f"evidence-dir is not empty: {path}")
    if not path.exists():
        parent = path.parent
        if not parent.exists() or not parent.is_dir():
            fail_preflight(f"evidence-dir parent does not exist: {parent}")
        try:
            path.mkdir()
        except OSError as exc:
            fail_preflight(f"cannot create evidence-dir {path}: {exc}")
    if not os.access(path, os.W_OK):
        fail_preflight(f"evidence-dir is not writable: {path}")
    return path


def check_dependencies():
    for name in ("incus", "curl", "jq", "ssh", "python3", "bash"):
        if shutil.which(name) is None:
            fail_preflight(f"missing dependency on PATH: {name}")
    if not TOPOLOGY.is_file():
        fail_preflight(f"missing topology helper: {TOPOLOGY}")
    if not PROBE.is_file():
        fail_preflight(f"missing probe script: {PROBE}")
    if not GATEWAY_KEY.is_file():
        fail_preflight(f"missing gateway ssh key: {GATEWAY_KEY}")


def check_ssh(cycle):
    sandbox = cycle.run(SSH_SANDBOX + ["true"], timeout=15)
    if sandbox.returncode != 0:
        fail_preflight(
            "ssh josh@sandbox01 failed: "
            + (
                sandbox.stderr.strip()
                or sandbox.stdout.strip()
                or f"exit {sandbox.returncode}"
            )
        )
    gateway = cycle.run(SSH_GATEWAY + ["true"], timeout=15)
    if gateway.returncode != 0:
        fail_preflight(
            "ssh vyos@10.0.0.2 failed: "
            + (
                gateway.stderr.strip()
                or gateway.stdout.strip()
                or f"exit {gateway.returncode}"
            )
        )

    project = cycle.run(["incus", "project", "show", f"{REMOTE}:{PROJECT}"], timeout=15)
    if project.returncode == 0:
        fail_preflight(
            f"refusing existing project {PROJECT}; destroy it explicitly first"
        )
    if not any(
        text in project.stderr.lower()
        for text in ("not found", "does not exist", "doesn't exist")
    ):
        fail_preflight(
            f"cannot establish that {PROJECT} is absent: {project.stderr.strip()}"
        )


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Create, probe, diagnose, and destroy the disposable OVN topology.",
        epilog="Invoke as spikes/ovn/probe.sh cycle --evidence-dir PATH [--keep-on-failure]. "
        "Does not stop OVN central, run keeper, or clear neighbor caches.",
    )
    parser.add_argument(
        "--evidence-dir",
        required=True,
        help="directory for cycle.json and capture logs",
    )
    parser.add_argument(
        "--keep-on-failure",
        action="store_true",
        help="leave owned resources in place when create/probe/capture fails",
    )
    args = parser.parse_args(argv)
    evidence = validate_evidence_dir(args.evidence_dir)
    check_dependencies()
    cycle = Cycle(evidence, args.keep_on_failure)
    print(f"cycle: evidence-dir={evidence}", file=sys.stderr)
    check_ssh(cycle)

    def interrupted(_signum, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    cycle.execute()


if __name__ == "__main__":
    main()
