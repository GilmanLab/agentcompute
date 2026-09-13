#!/usr/bin/env python3
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "PyYAML==6.0.3",
# ]
# ///
"""Validate pins and assemble Incus images.

Usage:
  uv run --locked --script images/build.py validate
  sudo env PATH="$PATH" uv run --locked --script images/build.py build \\
    --image router|runner|runner-publisher \\
    --work-dir <new-dir> --output-dir <new-dir>

--image defaults to router (unified tar.xz). runner and runner-publisher
emit split incus.tar.xz + disk.qcow2.
"""

from __future__ import annotations

import re
import argparse
import hashlib
import json
import os
import resource
import shutil
import subprocess
import sys
import tarfile
import time
import urllib.request
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

import yaml

IMAGES = Path(__file__).resolve().parent
PINS_PATH = IMAGES / "pins.yaml"
CATALOG_PATH = IMAGES / "catalog.yaml"
ROUTER_DIR = IMAGES / "router"
RECIPE_PATH = ROUTER_DIR / "distrobuilder.yaml"
RUNNER_DIR = IMAGES / "runner"
RUNNER_RECIPE_PATH = RUNNER_DIR / "distrobuilder.yaml"
IMAGE_NAMES = ("router", "runner", "runner-publisher")
PYYAML_VERSION = "6.0.3"
DISTROBUILDER_TAGS = (
    "containers_image_storage_stub,containers_image_docker_daemon_stub,"
    "containers_image_openpgp"
)
PINS_KEYS = {
    "schema_version",
    "architecture",
    "distrobuilder",
    "go",
    "incus",
    "alpine",
    "imgoci",
    "pyyaml",
    "runner",
}
SNAPSHOT_RE = re.compile(r"^[0-9]{8}T[0-9]{6}Z$")
PROXY_HTTP = "http://10.10.10.14:3128"
PROXY_NO = "10.10.10.14,127.0.0.1,localhost"
CATALOG_KEYS = {"schema_version", "images"}
SHA256_LEN = 64


class Error(RuntimeError):
    """Raised when validation or the build cannot continue."""


def load_yaml(path: Path) -> Any:
    if not path.is_file():
        raise Error(f"missing {path}")
    try:
        data = yaml.safe_load(path.read_text(encoding="utf-8"))
    except yaml.YAMLError as exc:
        raise Error(f"invalid YAML in {path}: {exc}") from exc
    return data


def require_mapping(value: Any, name: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise Error(f"{name} must be a mapping")
    return value


def require_list(value: Any, name: str) -> list[Any]:
    if not isinstance(value, list):
        raise Error(f"{name} must be a list")
    return value


def require_str(value: Any, name: str) -> str:
    if not isinstance(value, str) or not value:
        raise Error(f"{name} must be a non-empty string")
    return value


def require_bool(value: Any, name: str) -> bool:
    if not isinstance(value, bool):
        raise Error(f"{name} must be a boolean")
    return value


def require_sha256(value: Any, name: str) -> str:
    digest = require_str(value, name).lower()
    if len(digest) != SHA256_LEN or any(c not in "0123456789abcdef" for c in digest):
        raise Error(f"{name} must be a 64-character lowercase hex SHA-256")
    return digest


def require_https(url: str, name: str) -> str:
    parsed = urlparse(url)
    if parsed.scheme != "https" or not parsed.netloc:
        raise Error(f"{name} must be an https URL")
    return url


def extra_keys(data: dict[str, Any], allowed: set[str], name: str) -> None:
    extra = sorted(set(data) - allowed)
    if extra:
        raise Error(f"unknown keys in {name}: {', '.join(extra)}")


def pinned_filename(package: dict[str, Any]) -> str:
    url = require_str(package.get("url"), "alpine.packages[].url")
    return url.rsplit("/", 1)[-1]


def load_pins() -> dict[str, Any]:
    pins = require_mapping(load_yaml(PINS_PATH), str(PINS_PATH))
    extra_keys(pins, PINS_KEYS, str(PINS_PATH))
    if pins.get("schema_version") != 1:
        raise Error("pins.yaml schema_version must be 1")
    if pins.get("architecture") != "amd64":
        raise Error("pins.yaml architecture must be amd64")
    for name in ("distrobuilder", "go", "incus"):
        block = require_mapping(pins.get(name), name)
        extra_keys(block, {"version", "url", "sha256"}, name)
        require_str(block.get("version"), f"{name}.version")
        require_https(require_str(block.get("url"), f"{name}.url"), f"{name}.url")
        require_sha256(block.get("sha256"), f"{name}.sha256")
    alpine = require_mapping(pins.get("alpine"), "alpine")
    extra_keys(
        alpine,
        {"version", "url", "sha256", "requested", "packages", "base_packages"},
        "alpine",
    )
    require_str(alpine.get("version"), "alpine.version")
    require_https(require_str(alpine.get("url"), "alpine.url"), "alpine.url")
    require_sha256(alpine.get("sha256"), "alpine.sha256")
    requested = require_list(alpine.get("requested"), "alpine.requested")
    if not all(isinstance(item, str) and item for item in requested):
        raise Error("alpine.requested must be a list of non-empty strings")
    packages = require_list(alpine.get("packages"), "alpine.packages")
    names: set[str] = set()
    for index, item in enumerate(packages):
        package = require_mapping(item, f"alpine.packages[{index}]")
        extra_keys(package, {"name", "version", "url", "sha256"}, f"alpine.packages[{index}]")
        name = require_str(package.get("name"), f"alpine.packages[{index}].name")
        if name in names:
            raise Error(f"duplicate alpine package {name}")
        names.add(name)
        version = require_str(package.get("version"), f"alpine.packages[{index}].version")
        url = require_https(
            require_str(package.get("url"), f"alpine.packages[{index}].url"),
            f"alpine.packages[{index}].url",
        )
        require_sha256(package.get("sha256"), f"alpine.packages[{index}].sha256")
        filename = url.rsplit("/", 1)[-1]
        expected = f"{name}-{version}.apk"
        if filename != expected:
            raise Error(f"{url} filename {filename} does not match {expected}")
    missing_requested = [name for name in requested if name not in names]
    if missing_requested:
        raise Error("alpine.requested missing from packages: " + ", ".join(missing_requested))
    base_packages = require_list(alpine.get("base_packages"), "alpine.base_packages")
    if not all(isinstance(item, str) and item for item in base_packages):
        raise Error("alpine.base_packages must be a list of non-empty strings")
    missing_base = [name for name in base_packages if name not in names]
    if missing_base:
        raise Error("alpine.base_packages missing from packages: " + ", ".join(missing_base))
    imgoci = require_mapping(pins.get("imgoci"), "imgoci")
    extra_keys(imgoci, {"module", "version"}, "imgoci")
    require_str(imgoci.get("module"), "imgoci.module")
    require_str(imgoci.get("version"), "imgoci.version")
    pyyaml = require_mapping(pins.get("pyyaml"), "pyyaml")
    extra_keys(pyyaml, {"version"}, "pyyaml")
    if pyyaml.get("version") != PYYAML_VERSION:
        raise Error(f"pyyaml.version must be {PYYAML_VERSION}")
    load_runner_pins(pins)
    return pins

def load_runner_pins(pins: dict[str, Any]) -> None:
    runner = require_mapping(pins.get("runner"), "runner")
    extra_keys(runner, {"ubuntu", "actions_runner", "ca_certificates", "guest", "proxy"}, "runner")
    ubuntu = require_mapping(runner.get("ubuntu"), "runner.ubuntu")
    extra_keys(ubuntu, {"version", "release", "url", "sha256", "snapshot"}, "runner.ubuntu")
    require_str(ubuntu.get("version"), "runner.ubuntu.version")
    if ubuntu.get("release") != "noble":
        raise Error("runner.ubuntu.release must be noble")
    require_https(require_str(ubuntu.get("url"), "runner.ubuntu.url"), "runner.ubuntu.url")
    require_sha256(ubuntu.get("sha256"), "runner.ubuntu.sha256")
    snapshot = require_str(ubuntu.get("snapshot"), "runner.ubuntu.snapshot")
    if SNAPSHOT_RE.match(snapshot) is None:
        raise Error("runner.ubuntu.snapshot must be YYYYMMDDTHHMMSSZ")
    ca_certs = require_mapping(runner.get("ca_certificates"), "runner.ca_certificates")
    extra_keys(ca_certs, {"version", "url", "sha256"}, "runner.ca_certificates")
    ca_version = require_str(ca_certs.get("version"), "runner.ca_certificates.version")
    ca_url = require_https(
        require_str(ca_certs.get("url"), "runner.ca_certificates.url"),
        "runner.ca_certificates.url",
    )
    require_sha256(ca_certs.get("sha256"), "runner.ca_certificates.sha256")
    ca_filename = ca_url.rsplit("/", 1)[-1]
    expected_ca = f"ca-certificates_{ca_version}_all.deb"
    if ca_filename != expected_ca:
        raise Error(f"runner.ca_certificates.url filename {ca_filename} does not match {expected_ca}")
    if f"/{snapshot}/" not in ca_url:
        raise Error("runner.ca_certificates.url must be from the pinned Ubuntu snapshot")
    actions = require_mapping(runner.get("actions_runner"), "runner.actions_runner")
    extra_keys(actions, {"version", "url", "sha256"}, "runner.actions_runner")
    require_str(actions.get("version"), "runner.actions_runner.version")
    url = require_https(
        require_str(actions.get("url"), "runner.actions_runner.url"),
        "runner.actions_runner.url",
    )
    require_sha256(actions.get("sha256"), "runner.actions_runner.sha256")
    expected = f"actions-runner-linux-x64-{actions['version']}.tar.gz"
    if url.rsplit("/", 1)[-1] != expected:
        raise Error(f"runner.actions_runner.url filename must be {expected}")
    guest = require_mapping(runner.get("guest"), "runner.guest")
    extra_keys(guest, {"version", "origin", "tag", "files"}, "runner.guest")
    if guest.get("version") != "2.0.0" or guest.get("tag") != "v2.0.0":
        raise Error("runner.guest must pin incus-gh-runner v2.0.0")
    origin = require_https(require_str(guest.get("origin"), "runner.guest.origin"), "runner.guest.origin")
    if origin != "https://github.com/meigma/incus-gh-runner":
        raise Error("runner.guest.origin must be https://github.com/meigma/incus-gh-runner")
    files = require_list(guest.get("files"), "runner.guest.files")
    if len(files) != 5:
        raise Error("runner.guest.files must list the five v2.0.0 guest assets")
    seen: set[str] = set()
    for index, item in enumerate(files):
        entry = require_mapping(item, f"runner.guest.files[{index}]")
        extra_keys(entry, {"source", "path", "sha256"}, f"runner.guest.files[{index}]")
        source = require_str(entry.get("source"), f"runner.guest.files[{index}].source")
        path = require_str(entry.get("path"), f"runner.guest.files[{index}].path")
        require_sha256(entry.get("sha256"), f"runner.guest.files[{index}].sha256")
        if not source.startswith("guest/") or ".." in source or ".." in path:
            raise Error(f"invalid guest path {source} -> {path}")
        if not path.startswith("files/") or path in seen:
            raise Error(f"duplicate or invalid guest dest {path}")
        seen.add(path)
    proxy = require_mapping(runner.get("proxy"), "runner.proxy")
    extra_keys(proxy, {"http", "no_proxy"}, "runner.proxy")
    if proxy.get("http") != PROXY_HTTP:
        raise Error(f"runner.proxy.http must be {PROXY_HTTP}")
    if proxy.get("no_proxy") != PROXY_NO:
        raise Error(f"runner.proxy.no_proxy must be {PROXY_NO}")


def load_runner_recipe() -> dict[str, Any]:
    recipe = require_mapping(load_yaml(RUNNER_RECIPE_PATH), str(RUNNER_RECIPE_PATH))
    source = require_mapping(recipe.get("source"), "runner source")
    url = require_str(source.get("url"), "runner source.url")
    parsed = urlparse(url)
    if parsed.scheme != "file" or not parsed.path:
        raise Error("runner source.url must be a file:// seed path")
    packages = require_mapping(recipe.get("packages"), "runner packages")
    if packages.get("manager") != "apt":
        raise Error("runner packages.manager must be apt")
    if not require_bool(packages.get("update"), "runner packages.update"):
        raise Error("runner packages.update must be true")
    files = require_list(recipe.get("files"), "runner files")
    generators = {item.get("generator") for item in files if isinstance(item, dict)}
    if "incus-agent" not in generators:
        raise Error("runner recipe must include the incus-agent generator")
    fstab = next(
        (
            item
            for item in files
            if isinstance(item, dict) and item.get("path") == "/etc/fstab"
        ),
        None,
    )
    if not isinstance(fstab, dict) or "x-systemd.growfs" not in str(fstab.get("content") or ""):
        raise Error("runner /etc/fstab must set x-systemd.growfs")
    return recipe


def validate_guest_files(pins: dict[str, Any]) -> None:
    for entry in pins["runner"]["guest"]["files"]:
        path = RUNNER_DIR / entry["path"]
        if not path.is_file():
            raise Error(f"missing vendored guest file {path}")
        digest = sha256_file(path)
        if digest != entry["sha256"]:
            raise Error(f"sha256 mismatch for {path}: got {digest} want {entry['sha256']}")
    wrapper = RUNNER_DIR / "files/usr/local/sbin/agentcompute-build"
    sudoers = RUNNER_DIR / "files/etc/sudoers.d/agentcompute-build"
    proxy = RUNNER_DIR / "files/etc/agentcompute-build/proxy.env"
    for path in (wrapper, sudoers, proxy):
        if not path.is_file():
            raise Error(f"missing publisher file {path}")
    text = proxy.read_text(encoding="utf-8")
    expected = (
        f"HTTP_PROXY={PROXY_HTTP}\n"
        f"HTTPS_PROXY={PROXY_HTTP}\n"
        f"http_proxy={PROXY_HTTP}\n"
        f"https_proxy={PROXY_HTTP}\n"
        f"NO_PROXY={PROXY_NO}\n"
        f"no_proxy={PROXY_NO}\n"
    )
    if text != expected:
        raise Error("publisher proxy.env does not match runner.proxy pins")



def recipe_package_filenames(recipe: dict[str, Any]) -> list[str]:
    packages = require_mapping(recipe.get("packages"), "recipe packages")
    if packages.get("manager") != "apk":
        raise Error("recipe packages.manager must be apk")
    if require_bool(packages.get("update"), "packages.update"):
        raise Error("recipe packages.update must be false")
    filenames: list[str] = []
    for index, item in enumerate(require_list(packages.get("sets"), "packages.sets")):
        package_set = require_mapping(item, f"packages.sets[{index}]")
        flags = package_set.get("flags") or []
        if "--no-network" not in flags:
            raise Error(f"packages.sets[{index}] must set --no-network")
        for path in require_list(package_set.get("packages"), f"packages.sets[{index}].packages"):
            text = require_str(path, f"packages.sets[{index}].packages[]")
            prefix = "/packages/"
            if not text.startswith(prefix) or text != text.strip() or "/" in text[len(prefix) :]:
                raise Error(f"recipe package path must be {prefix}<file>.apk: {text}")
            filenames.append(text[len(prefix) :])
    if not filenames:
        raise Error("recipe installs no packages")
    return filenames


def load_recipe() -> dict[str, Any]:
    recipe = require_mapping(load_yaml(RECIPE_PATH), str(RECIPE_PATH))
    source = require_mapping(recipe.get("source"), "source")
    url = require_str(source.get("url"), "source.url")
    parsed = urlparse(url)
    if parsed.scheme != "file" or not parsed.path:
        raise Error("recipe source.url must be a file:// seed path")
    return recipe


def validate_package_closure(pins: dict[str, Any], recipe: dict[str, Any]) -> None:
    pinned = [pinned_filename(package) for package in pins["alpine"]["packages"]]
    recipe_files = recipe_package_filenames(recipe)
    if sorted(pinned) != sorted(recipe_files):
        only_pins = sorted(set(pinned) - set(recipe_files))
        only_recipe = sorted(set(recipe_files) - set(pinned))
        details = []
        if only_pins:
            details.append("in pins only: " + ", ".join(only_pins))
        if only_recipe:
            details.append("in recipe only: " + ", ".join(only_recipe))
        raise Error("package closure mismatch: " + "; ".join(details))
    if len(set(recipe_files)) != len(recipe_files):
        raise Error("recipe package list has duplicates")


def validate_catalog() -> None:
    if not CATALOG_PATH.exists():
        return
    catalog = require_mapping(load_yaml(CATALOG_PATH), str(CATALOG_PATH))
    extra_keys(catalog, CATALOG_KEYS, str(CATALOG_PATH))
    if "images" not in catalog:
        raise Error("catalog.yaml must define images")
    images = catalog.get("images")
    if not isinstance(images, (list, dict)):
        raise Error("catalog.yaml images must be a list or mapping")


def validate() -> dict[str, Any]:
    pins = load_pins()
    recipe = load_recipe()
    validate_package_closure(pins, recipe)
    load_runner_recipe()
    validate_guest_files(pins)
    validate_catalog()
    return pins


def require_new_dir(path: Path, name: str) -> Path:
    resolved = path.expanduser()
    if not resolved.is_absolute():
        resolved = Path.cwd() / resolved
    resolved = resolved.resolve(strict=False)
    if resolved.exists():
        raise Error(f"{name} already exists: {resolved}")
    parent = resolved.parent
    if not parent.is_dir():
        raise Error(f"{name} parent does not exist: {parent}")
    resolved.mkdir(mode=0o700)
    resolved.chmod(0o700)
    return resolved


def sha256_file(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


def download(url: str, dest: Path, digest: str) -> None:
    hasher = hashlib.sha256()
    partial = dest.with_name(dest.name + ".partial")
    request = urllib.request.Request(url, method="GET")
    try:
        with urllib.request.urlopen(request) as response, partial.open("wb") as out:
            while True:
                chunk = response.read(1024 * 1024)
                if not chunk:
                    break
                hasher.update(chunk)
                out.write(chunk)
    except OSError as exc:
        partial.unlink(missing_ok=True)
        raise Error(f"download failed for {url}: {exc}") from exc
    got = hasher.hexdigest()
    if got != digest:
        partial.unlink(missing_ok=True)
        raise Error(f"sha256 mismatch for {url}: got {got} want {digest}")
    partial.replace(dest)


def run_checked(command: list[str], **kwargs: Any) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(command, check=True, text=True, **kwargs)
    except FileNotFoundError as exc:
        raise Error(f"missing command: {command[0]}") from exc
    except subprocess.CalledProcessError as exc:
        detail = (exc.stderr or exc.stdout or "").strip()
        suffix = f"\n{detail}" if detail else ""
        raise Error(f"command failed: {' '.join(command)}{suffix}") from exc


def first_line(command: list[str]) -> str:
    try:
        result = subprocess.run(command, check=True, text=True, capture_output=True)
    except (FileNotFoundError, subprocess.CalledProcessError) as exc:
        raise Error(f"unable to read version from {command[0]}") from exc
    line = (result.stdout or result.stderr).splitlines()
    if not line:
        raise Error(f"empty version output from {command[0]}")
    return line[0]


def scratch_bytes(root: Path) -> int:
    total = 0
    for dirpath, _dirnames, filenames in os.walk(root, followlinks=False):
        for name in filenames:
            path = os.path.join(dirpath, name)
            try:
                total += os.lstat(path).st_blocks * 512
            except OSError:
                continue
    return total


def rss_kib() -> int:
    return int(resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss)


def extract_tar(archive: Path, dest: Path) -> None:
    dest.mkdir(mode=0o700, exist_ok=True)
    dest.chmod(0o700)
    run_checked(["tar", "-xzf", str(archive), "-C", str(dest)])


def require_command(name: str) -> None:
    if shutil.which(name) is None:
        raise Error(f"missing command: {name}")


def compile_distrobuilder(pins: dict[str, Any], work: Path, downloads: Path) -> tuple[Path, float, int]:
    go_archive = downloads / pins["go"]["url"].rsplit("/", 1)[-1]
    distro_archive = downloads / pins["distrobuilder"]["url"].rsplit("/", 1)[-1]
    if not go_archive.is_file() or not distro_archive.is_file():
        raise Error("go/distrobuilder archives missing from downloads")
    compile_started = time.monotonic()
    extract_tar(go_archive, work)
    extract_tar(distro_archive, work)
    go_bin = work / "go" / "bin"
    distro_src = work / f"distrobuilder-{pins['distrobuilder']['version']}"
    distro_bin = work / "distrobuilder"
    if not (go_bin / "go").is_file():
        raise Error("go toolchain extract did not produce go/bin/go")
    if not distro_src.is_dir():
        raise Error(f"distrobuilder extract did not produce {distro_src.name}")
    env = os.environ.copy()
    env["PATH"] = f"{go_bin}{os.pathsep}{env.get('PATH', '')}"
    env["GOTOOLCHAIN"] = "local"
    env["GOCACHE"] = str(work / "gocache")
    env["GOMODCACHE"] = str(work / "gomod")
    (work / "gocache").mkdir(mode=0o700)
    (work / "gomod").mkdir(mode=0o700)
    run_checked(
        [
            str(go_bin / "go"),
            "build",
            "-mod=vendor",
            "-trimpath",
            f"-tags={DISTROBUILDER_TAGS}",
            "-o",
            str(distro_bin),
            "./distrobuilder",
        ],
        cwd=distro_src,
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
    )
    if not distro_bin.is_file():
        raise Error("distrobuilder compile did not produce a binary")
    return distro_bin, round(time.monotonic() - compile_started, 3), rss_kib()


def inject_seed_ca_certificates(deb: Path, seed: Path, work: Path) -> None:
    extracted = work / "ca-certificates"
    if extracted.exists():
        shutil.rmtree(extracted)
    extracted.mkdir(mode=0o700)
    run_checked(["dpkg-deb", "-x", str(deb), str(extracted)])
    mozilla = extracted / "usr" / "share" / "ca-certificates" / "mozilla"
    if not mozilla.is_dir():
        raise Error("ca-certificates deb has no Mozilla cert directory")
    certs = sorted(path for path in mozilla.iterdir() if path.is_file() and path.suffix == ".crt")
    if not certs:
        raise Error("ca-certificates deb has no Mozilla .crt files")
    dest_mozilla = seed / "usr" / "share" / "ca-certificates" / "mozilla"
    dest_mozilla.mkdir(parents=True, exist_ok=True)
    parts: list[str] = []
    for cert in certs:
        target = dest_mozilla / cert.name
        shutil.copyfile(cert, target, follow_symlinks=False)
        if sha256_file(target) != sha256_file(cert):
            raise Error(f"seed copy changed bytes: {cert.name}")
        text = target.read_text(encoding="utf-8")
        if "BEGIN CERTIFICATE" not in text:
            raise Error(f"Mozilla cert {cert.name} is not a PEM certificate")
        if not text.endswith("\n"):
            text += "\n"
        parts.append(text)
    dest_bundle = seed / "etc" / "ssl" / "certs"
    dest_bundle.mkdir(parents=True, exist_ok=True)
    (dest_bundle / "ca-certificates.crt").write_text("".join(parts), encoding="utf-8")


def host_proxy() -> str | None:
    for key in ("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"):
        value = os.environ.get(key)
        if value:
            return value
    return None


def build_vm(
    pins: dict[str, Any], work: Path, output: Path, tools: dict[str, str], image: str
) -> dict[str, Any]:
    variant = "publisher" if image == "runner-publisher" else "runner"
    downloads = work / "downloads"
    downloads.mkdir(mode=0o700)
    download_started = time.monotonic()
    ubuntu = pins["runner"]["ubuntu"]
    actions = pins["runner"]["actions_runner"]
    ca_certs = pins["runner"]["ca_certificates"]
    ubuntu_archive = downloads / ubuntu["url"].rsplit("/", 1)[-1]
    runner_archive = downloads / actions["url"].rsplit("/", 1)[-1]
    go_archive = downloads / pins["go"]["url"].rsplit("/", 1)[-1]
    distro_archive = downloads / pins["distrobuilder"]["url"].rsplit("/", 1)[-1]
    ca_deb = downloads / ca_certs["url"].rsplit("/", 1)[-1]
    download(pins["go"]["url"], go_archive, pins["go"]["sha256"])
    download(pins["distrobuilder"]["url"], distro_archive, pins["distrobuilder"]["sha256"])
    download(ubuntu["url"], ubuntu_archive, ubuntu["sha256"])
    download(ca_certs["url"], ca_deb, ca_certs["sha256"])
    download(actions["url"], runner_archive, actions["sha256"])
    download_wall = round(time.monotonic() - download_started, 3)
    distro_bin, compile_wall, compile_rss = compile_distrobuilder(pins, work, downloads)

    # Guest files need normal modes; their host work directory remains private.
    os.umask(0o022)
    seed = work / "seed"
    seed.mkdir()
    run_checked(["tar", "-xzf", str(ubuntu_archive), "-C", str(seed)])
    # This directory becomes guest /, which service users must traverse.
    seed.chmod(0o755)
    inject_seed_ca_certificates(ca_deb, seed, work)
    apt_dir = seed / "etc" / "apt" / "apt.conf.d"
    apt_dir.mkdir(parents=True, exist_ok=True)
    (apt_dir / "50agentcompute-snapshot").write_text(
        'Acquire::Check-Valid-Until "false";\n'
        'Acquire::Languages "none";\n',
        encoding="utf-8",
    )
    sources_dir = seed / "etc" / "apt" / "sources.list.d"
    sources_dir.mkdir(parents=True, exist_ok=True)
    (sources_dir / "ubuntu.sources").write_text(
        "Types: deb\n"
        f"URIs: https://snapshot.ubuntu.com/ubuntu/{ubuntu['snapshot']}\n"
        "Suites: noble noble-updates noble-security\n"
        "Components: main universe restricted\n"
        "Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n",
        encoding="utf-8",
    )
    sources_list = seed / "etc" / "apt" / "sources.list"
    if sources_list.exists() or sources_list.is_symlink():
        sources_list.unlink()
    sources_list.write_text("", encoding="utf-8")
    proxy = host_proxy()
    if proxy:
        (apt_dir / "99agentcompute-build-proxy").write_text(
            f'Acquire::http::Proxy "{proxy}";\nAcquire::https::Proxy "{proxy}";\n',
            encoding="utf-8",
        )
    cache = seed / "var" / "cache" / "agentcompute"
    cache.mkdir(parents=True, exist_ok=True)
    seeded_runner = cache / "actions-runner.tar.gz"
    try:
        os.link(runner_archive, seeded_runner)
    except OSError:
        shutil.copyfile(runner_archive, seeded_runner, follow_symlinks=False)
    if sha256_file(seeded_runner) != sha256_file(runner_archive):
        raise Error("seed copy changed actions-runner archive bytes")
    seed_tar = work / "seed.tar"
    run_checked(["tar", "-cf", str(seed_tar), "-C", str(seed), "."])
    shutil.rmtree(seed)

    cache_dir = work / "cache"
    cache_dir.mkdir(mode=0o700)
    command = [
        str(distro_bin),
        "build-incus",
        str(RUNNER_RECIPE_PATH),
        str(output),
        "--vm",
        "--type=split",
        "--compression=xz",
        "--disable-overlay",
        f"--cache-dir={cache_dir}",
        "-o",
        f"source.url={seed_tar.resolve().as_uri()}",
        "-o",
        f"image.variant={variant}",
        "-o",
        f"image.name={image}",
    ]
    assemble_started = time.monotonic()
    peak_scratch = scratch_bytes(work)
    log_path = work / "build.log"
    with log_path.open("w", encoding="utf-8") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, cwd=RUNNER_DIR)
        while process.poll() is None:
            peak_scratch = max(peak_scratch, scratch_bytes(work))
            time.sleep(0.1)
        peak_scratch = max(peak_scratch, scratch_bytes(work))
    assemble_wall = round(time.monotonic() - assemble_started, 3)
    assemble_rss = rss_kib()
    if process.returncode:
        sys.stderr.write(log_path.read_text(encoding="utf-8", errors="replace"))
        raise Error(f"distrobuilder exited {process.returncode}")

    metadata = output / "incus.tar.xz"
    disk = output / "disk.qcow2"
    if not metadata.is_file() or not disk.is_file():
        raise Error(f"missing split VM artifacts in {output}")
    try:
        info = json.loads(
            run_checked(["qemu-img", "info", "--output=json", str(disk)], capture_output=True).stdout
        )
    except Error:
        raise
    virtual = int(info.get("virtual-size") or 0)
    if virtual <= 0:
        raise Error(f"{disk} has no virtual size")
    metrics = {
        "image": image,
        "variant": variant,
        "download_wall_seconds": download_wall,
        "compile_wall_seconds": compile_wall,
        "assemble_wall_seconds": assemble_wall,
        "download_includes": (
            "https fetch and sha256 of go, vendored distrobuilder source, "
            "ubuntu-base, snapshot ca-certificates, and the Actions Runner archive"
        ),
        "compile_includes": (
            "extract go+distrobuilder and go build -mod=vendor "
            f"-tags={DISTROBUILDER_TAGS}; excludes download and assemble"
        ),
        "assemble_includes": (
            "ubuntu-base seed plus distrobuilder build-incus --vm --type=split "
            "--compression=xz --disable-overlay; excludes download and compile"
        ),
        "peak_rss_kib": assemble_rss,
        "compile_peak_rss_kib": compile_rss,
        "scratch_high_water_bytes": peak_scratch,
        "scratch_sampling_seconds": 0.1,
        "artifacts": {
            "incus.tar.xz": {
                "bytes": metadata.stat().st_size,
                "sha256": sha256_file(metadata),
            },
            "disk.qcow2": {
                "bytes": disk.stat().st_size,
                "sha256": sha256_file(disk),
                "virtual_bytes": virtual,
            },
        },
        "tools": tools,
        "ubuntu_snapshot": ubuntu["snapshot"],
        "actions_runner_version": actions["version"],
        "guest_version": pins["runner"]["guest"]["version"],
    }
    (output / "metrics.json").write_text(json.dumps(metrics, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(metrics, indent=2))
    return metrics

def build(work_dir: Path, output_dir: Path, image: str = "router") -> dict[str, Any]:
    pins = validate()
    info = os.uname()
    if info.sysname != "Linux" or info.machine not in {"x86_64", "amd64"}:
        raise Error(f"build requires Linux amd64, not {info.sysname} {info.machine}")
    if os.geteuid() != 0:
        raise Error("build requires root")
    if work_dir.expanduser().resolve(strict=False) == output_dir.expanduser().resolve(strict=False):
        raise Error("work-dir and output-dir must be different")
    os.umask(0o077)
    work = require_new_dir(work_dir, "--work-dir")
    output = require_new_dir(output_dir, "--output-dir")
    tools = {
        "tar": first_line(["tar", "--version"]),
        "xz": first_line(["xz", "--version"]),
        "gcc": first_line(["gcc", "--version"]),
    }
    if image not in IMAGE_NAMES:
        raise Error(f"unknown image {image}")
    if image != "router":
        for name in ("btrfs", "qemu-img", "sgdisk", "mkfs.ext4", "mkfs.vfat", "losetup", "rsync", "blkid", "mount", "dpkg-deb"):
            require_command(name)
        tools["qemu-img"] = first_line(["qemu-img", "--version"])
        tools["dpkg-deb"] = first_line(["dpkg-deb", "--version"])
        return build_vm(pins, work, output, tools, image)
    downloads = work / "downloads"
    packages_dir = work / "packages"
    downloads.mkdir(mode=0o700)
    packages_dir.mkdir(mode=0o700)

    download_started = time.monotonic()
    go_archive = downloads / pins["go"]["url"].rsplit("/", 1)[-1]
    distro_archive = downloads / pins["distrobuilder"]["url"].rsplit("/", 1)[-1]
    miniroot_archive = downloads / pins["alpine"]["url"].rsplit("/", 1)[-1]
    download(pins["go"]["url"], go_archive, pins["go"]["sha256"])
    download(pins["distrobuilder"]["url"], distro_archive, pins["distrobuilder"]["sha256"])
    download(pins["alpine"]["url"], miniroot_archive, pins["alpine"]["sha256"])
    apk_files: list[Path] = []
    for package in pins["alpine"]["packages"]:
        filename = pinned_filename(package)
        dest = packages_dir / filename
        download(package["url"], dest, package["sha256"])
        apk_files.append(dest)
    download_wall = round(time.monotonic() - download_started, 3)

    compile_started = time.monotonic()
    extract_tar(go_archive, work)
    extract_tar(distro_archive, work)
    go_bin = work / "go" / "bin"
    distro_src = work / f"distrobuilder-{pins['distrobuilder']['version']}"
    distro_bin = work / "distrobuilder"
    if not (go_bin / "go").is_file():
        raise Error("go toolchain extract did not produce go/bin/go")
    if not distro_src.is_dir():
        raise Error(f"distrobuilder extract did not produce {distro_src.name}")
    env = os.environ.copy()
    env["PATH"] = f"{go_bin}{os.pathsep}{env.get('PATH', '')}"
    env["GOTOOLCHAIN"] = "local"
    env["GOCACHE"] = str(work / "gocache")
    env["GOMODCACHE"] = str(work / "gomod")
    (work / "gocache").mkdir(mode=0o700)
    (work / "gomod").mkdir(mode=0o700)
    run_checked(
        [
            str(go_bin / "go"),
            "build",
            "-mod=vendor",
            "-trimpath",
            f"-tags={DISTROBUILDER_TAGS}",
            "-o",
            str(distro_bin),
            "./distrobuilder",
        ],
        cwd=distro_src,
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
    )
    if not distro_bin.is_file():
        raise Error("distrobuilder compile did not produce a binary")
    compile_wall = round(time.monotonic() - compile_started, 3)
    compile_rss = rss_kib()

    seed = work / "seed"
    seed.mkdir(mode=0o700)
    run_checked(["tar", "-xzf", str(miniroot_archive), "-C", str(seed)])
    seed_packages = seed / "packages"
    seed_packages.mkdir(mode=0o700)
    for apk in apk_files:
        target = seed_packages / apk.name
        try:
            os.link(apk, target)
        except OSError:
            shutil.copyfile(apk, target, follow_symlinks=False)
        if sha256_file(target) != sha256_file(apk):
            raise Error(f"seed copy changed bytes: {apk.name}")
    seed_tar = work / "seed.tar"
    run_checked(["tar", "-cf", str(seed_tar), "-C", str(seed), "."])
    shutil.rmtree(seed)

    cache = work / "cache"
    cache.mkdir(mode=0o700)
    command = [
        str(distro_bin),
        "build-incus",
        str(RECIPE_PATH),
        str(output),
        "--type=unified",
        "--compression=xz-1",
        "--disable-overlay",
        f"--cache-dir={cache}",
        "-o",
        f"source.url={seed_tar.resolve().as_uri()}",
    ]
    assemble_started = time.monotonic()
    peak_scratch = scratch_bytes(work)
    log_path = work / "build.log"
    # The private 0700 work tree is already in place; distrobuilder must
    # generate files inside the image with normal 0644/0755 modes, not 0600.
    os.umask(0o022)
    with log_path.open("w", encoding="utf-8") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, cwd=ROUTER_DIR)
        while process.poll() is None:
            peak_scratch = max(peak_scratch, scratch_bytes(work))
            time.sleep(0.1)
        peak_scratch = max(peak_scratch, scratch_bytes(work))
    assemble_wall = round(time.monotonic() - assemble_started, 3)
    assemble_rss = rss_kib()
    if process.returncode:
        sys.stderr.write(log_path.read_text(encoding="utf-8", errors="replace"))
        raise Error(f"distrobuilder exited {process.returncode}")

    artifact = output / "router.tar.xz"
    if not artifact.is_file():
        raise Error(f"missing {artifact}")
    try:
        with tarfile.open(artifact) as archive:
            decoded = sum(member.size for member in archive)
    except tarfile.TarError as exc:
        raise Error(f"invalid unified image {artifact}: {exc}") from exc
    if decoded <= 0:
        raise Error(f"{artifact} decoded to 0 bytes")
    metrics = {
        "download_wall_seconds": download_wall,
        "compile_wall_seconds": compile_wall,
        "assemble_wall_seconds": assemble_wall,
        "download_includes": (
            "https fetch and sha256 of go, vendored distrobuilder source, "
            "alpine miniroot, and every pinned apk"
        ),
        "compile_includes": (
            "extract go+distrobuilder and go build -mod=vendor "
            f"-tags={DISTROBUILDER_TAGS}; excludes download and assemble"
        ),
        "assemble_includes": (
            "seed tar plus distrobuilder build-incus --type=unified "
            "--compression=xz-1 --disable-overlay; excludes download and compile"
        ),
        "peak_rss_kib": assemble_rss,
        "compile_peak_rss_kib": compile_rss,
        "scratch_high_water_bytes": peak_scratch,
        "scratch_sampling_seconds": 0.1,
        "artifact_bytes": artifact.stat().st_size,
        "artifact_sha256": sha256_file(artifact),
        "decoded_bytes": decoded,
        "tools": tools,
    }
    (output / "metrics.json").write_text(json.dumps(metrics, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(metrics, indent=2))
    return metrics


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("validate", help="check pins, recipe closure, and catalog keys")
    build_cmd = sub.add_parser("build", help="download, compile, and assemble an image")
    build_cmd.add_argument("--image", choices=IMAGE_NAMES, default="router")
    build_cmd.add_argument("--work-dir", type=Path, required=True)
    build_cmd.add_argument("--output-dir", type=Path, required=True)
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    try:
        args = parse_args(argv)
        if args.command == "validate":
            validate()
        else:
            build(args.work_dir, args.output_dir, args.image)
    except Error as exc:
        print(f"images/build.py: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
