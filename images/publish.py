#!/usr/bin/env python3
"""Build, qualify, publish, and independently fetch protected image releases."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time

IMAGES = ("router", "runner", "runner-publisher", "ubuntu-24.04-desktop")
REPOSITORY = "ghcr.io/gilmanlab/agentcompute"


def run(*args: str) -> None:
    subprocess.run(args, check=True, stdout=sys.stderr)


def capture(*args: str) -> str:
    return subprocess.check_output(args, text=True).strip()


def digest(path: Path) -> str:
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def definition_tree() -> str:
    entries = capture("git", "ls-tree", "HEAD:images").splitlines()
    definition = "\n".join(row for row in entries if row.split("\t", 1)[1] not in {"catalog.yaml", "README.md"}) + "\n"
    return subprocess.check_output(["git", "mktree"], input=definition, text=True).strip()


def qualify(name: str, output: Path, evidence: Path) -> None:
    if name == "router":
        smoke = capture("bash", "images/smoke.sh", "--file", str(output / "router.tar.xz"),
                        "--remote", "nas01", "--project", "image-build",
                        "--suffix", f"ci-{os.environ['GITHUB_RUN_ID']}-{os.environ['GITHUB_RUN_ATTEMPT']}",
                        "--log", str(evidence / "router-smoke.log"))
        (evidence / "router-smoke.json").write_text(smoke + "\n")
        return
    metadata = str(output / "incus.tar.xz")
    disk = str(output / "disk.qcow2")
    if name == "ubuntu-24.04-desktop":
        run(sys.executable, "images/ubuntu-24.04-desktop/smoke.py", "--metadata", metadata,
            "--disk", disk, "--remote", "nas01", "--project", "image-build",
            "--profile", "runner-smoke", "--evidence", str(evidence / f"{name}-smoke"))
        return
    run(sys.executable, "images/runner/smoke.py", "--metadata", metadata,
        "--disk", disk, "--remote", "nas01", "--project", "image-build",
        "--profile", "runner-smoke", "--evidence", str(evidence / f"{name}-smoke"))


def bake(sha: str, evidence: Path, publisher: str) -> None:
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("source SHA must be 40 lowercase hexadecimal characters")
    run("git", "merge-base", "--is-ancestor", sha, "origin/master")
    if capture("git", "rev-parse", "HEAD") != sha:
        raise ValueError("checkout does not match the approved source SHA")
    version = f"tree-{definition_tree()[:12]}"
    evidence.mkdir(parents=True, exist_ok=True)
    releases = []
    for name in IMAGES:
        started = time.monotonic()
        reference = f"{REPOSITORY}/{name}:{version}"
        existing = json.loads(capture(publisher, "inspect", "--ref", reference))
        record = {"name": name, "reference": reference, "version": version, "source_sha": sha}
        output = None
        if existing["exists"]:
            record.update(digest=existing["digest"], skipped=True)
            print(f"{name}: release exists; skipping assembly and publication", file=sys.stderr)
        else:
            result = json.loads(capture("sudo", "/usr/local/sbin/agentcompute-build", sha, name))
            output = Path(result["output_dir"])
            metric = json.loads((output / "metrics.json").read_text())
            (evidence / f"{name}-metrics.json").write_text(json.dumps(metric, indent=2) + "\n")
            qualify(name, output, evidence)
            files = (["--file", str(output / "router.tar.xz")] if name == "router" else
                     ["--metadata", str(output / "incus.tar.xz"), "--disk", str(output / "disk.qcow2")])
            published = json.loads(capture(publisher, "publish", "--ref", reference,
                                           "--version", version, "--name", name, *files))
            record.update(digest=published["digest"], skipped=False)
        immutable = f"{REPOSITORY}/{name}@{record['digest']}"
        with tempfile.TemporaryDirectory(prefix=f"{name}-fetch-") as temporary:
            fetched = Path(temporary)
            if name == "router":
                capture(publisher, "fetch", "--ref", immutable, "--output", str(fetched / "router.tar.xz"))
                filenames = ("router.tar.xz",)
            else:
                fetched = fetched / "vm"
                capture(publisher, "fetch", "--vm", "--ref", immutable, "--output", str(fetched))
                filenames = ("incus.tar.xz", "disk.qcow2")
            if output is None:
                # A prior run may have published before its fetch-back completed.
                qualify(name, fetched, evidence)
            else:
                for filename in filenames:
                    if digest(fetched / filename) != digest(output / filename):
                        raise ValueError(f"published {name}/{filename} differs from qualified image")
        record["wall_seconds"] = round(time.monotonic() - started, 3)
        releases.append(record)
        (evidence / "releases.json").write_text(json.dumps(releases, indent=2) + "\n")
    print(json.dumps(releases))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    build = commands.add_parser("bake")
    build.add_argument("--sha", required=True)
    build.add_argument("--evidence", required=True, type=Path)
    build.add_argument("--publisher", required=True)
    args = parser.parse_args()
    bake(args.sha, args.evidence, args.publisher)


if __name__ == "__main__":
    main()
