#!/usr/bin/env python3
"""Open a catalog-only PR from qualified releases without changing public master."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

import yaml

REPO = "GilmanLab/agentcompute"
NAMESPACE = "ghcr.io/gilmanlab/agentcompute"
IMAGES = ("router", "runner", "runner-publisher", "ubuntu-24.04-desktop")
DESKTOP_IMAGE = "ubuntu-24.04-desktop"
DESKTOP_CATALOG = "ubuntu/24.04/desktop"


def capture(*args: str, cwd: Path | None = None) -> str:
    return subprocess.check_output(args, cwd=cwd, text=True).strip()


def run(*args: str, cwd: Path) -> None:
    subprocess.run(args, cwd=cwd, check=True)


def catalog_name(image: str) -> str:
    if image == DESKTOP_IMAGE:
        return DESKTOP_CATALOG
    return image


def new_entry(image: str) -> dict:
    if image == DESKTOP_IMAGE:
        return {
            "name": DESKTOP_CATALOG,
            "os": "ubuntu",
            "version": "24.04",
            "kinds": ["vm"],
            "kind": "vm",
            "desktop": True,
            "cpus": 4,
            "memory_mb": 8192,
            "disk_gb": 40,
        }
    return {
        "name": image,
        "os": "ubuntu",
        "version": "24.04",
        "kinds": ["vm"],
        "kind": "vm",
        "cpus": 4,
        "memory_mb": 8192,
        "disk_gb": 40,
    }


def apply_desktop_contract(entry: dict) -> None:
    entry["name"] = DESKTOP_CATALOG
    entry["os"] = "ubuntu"
    entry["version"] = "24.04"
    entry["kinds"] = ["vm"]
    entry["kind"] = "vm"
    entry["desktop"] = True
    entry["cpus"] = 4
    entry["memory_mb"] = 8192
    entry["disk_gb"] = 40


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sha", required=True)
    parser.add_argument("--releases", required=True, type=Path)
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9a-f]{40}", args.sha):
        raise ValueError("invalid source SHA")
    releases = json.loads(args.releases.read_text())
    if {item["name"] for item in releases} != set(IMAGES) or len(releases) != len(IMAGES):
        raise ValueError("all four qualified image releases are required")
    for item in releases:
        if item["source_sha"] != args.sha or not re.fullmatch(r"sha256:[0-9a-f]{64}", item["digest"]):
            raise ValueError("invalid qualified release identity")
    branch = f"images/catalog-{args.sha}"
    existing = json.loads(capture("gh", "pr", "list", "--repo", REPO, "--head", branch,
                                  "--state", "open", "--json", "url"))
    if existing:
        print(existing[0]["url"])
        return
    with tempfile.TemporaryDirectory(prefix="image-catalog-") as temporary:
        root = Path(temporary)
        run("git", "clone", "--branch", "master", f"https://github.com/{REPO}.git", str(root / "source"), cwd=root)
        source = root / "source"
        expected_head = capture("git", "for-each-ref", "--format=%(objectname)",
                                f"refs/remotes/origin/{branch}", cwd=source)
        run("git", "merge-base", "--is-ancestor", args.sha, "origin/master", cwd=source)
        run("git", "diff", "--exit-code", args.sha, "origin/master", "--", "images", "cmd/image-publish",
            "go.mod", "go.sum", ":(exclude)images/catalog.yaml", ":(exclude)images/README.md", cwd=source)
        path = source / "images/catalog.yaml"
        original = path.read_text()
        catalog = yaml.safe_load(original)
        entries = {entry["name"]: entry for entry in catalog["images"]}
        changed = False
        for item in releases:
            name = item["name"]
            entry_name = catalog_name(name)
            entry = entries.get(entry_name)
            if entry is None:
                if name == "router":
                    raise ValueError("router catalog entry is missing")
                entry = new_entry(name)
                catalog["images"].append(entry)
                entries[entry_name] = entry
                changed = True
            if name == DESKTOP_IMAGE:
                before = json.dumps(entry, sort_keys=True, default=str)
                apply_desktop_contract(entry)
                changed |= json.dumps(entry, sort_keys=True, default=str) != before
            reference = f"{NAMESPACE}/{name}@{item['digest']}"
            changed |= entry.get("reference") != reference
            entry["reference"] = reference
        if not changed:
            print("Catalog already records every qualified digest; no PR needed")
            return
        # Keep the contract header; the new PR supplies current per-entry provenance.
        header = re.match(r"(?:#[^\n]*\n|\n)*", original).group(0)
        path.write_text(header + yaml.safe_dump(catalog, sort_keys=False))
        run("git", "switch", "--create", branch, cwd=source)
        run("git", "add", "--", "images/catalog.yaml", cwd=source)
        bot = os.environ["IMAGES_APP_SLUG"] + "[bot]"
        run("git", "-c", f"user.name={bot}", "-c", f"user.email={bot}@users.noreply.github.com",
            "commit", "--only", "-m", "feat(images): promote qualified lab image digests", "--", "images/catalog.yaml", cwd=source)
        run("git", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
            "push", f"--force-with-lease=refs/heads/{branch}:{expected_head}",
            "origin", f"HEAD:refs/heads/{branch}", cwd=source)
        body = f"Images built from public master commit `{args.sha}`.\n\n"
        body += "\n".join(f"- `{item['name']}`: `{item['digest']}`" for item in releases)
        body += f"\n\nBake evidence: https://github.com/GilmanLab/agentcompute-images/actions/runs/{os.environ['GITHUB_RUN_ID']}\n"
        print(capture("gh", "pr", "create", "--repo", REPO, "--base", "master", "--head", branch,
                      "--title", "feat(images): promote qualified lab image digests", "--body", body, cwd=source))


if __name__ == "__main__":
    main()
