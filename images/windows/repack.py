#!/usr/bin/env python3
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "PyYAML==6.0.3",
# ]
# ///
"""Stage the pinned inputs for a Windows bake: repack the installer ISO and
build the provisioning payload ISO.

Usage:
  uv run --locked --script images/windows/repack.py validate
  sudo env PATH="$PATH" uv run --locked --script images/windows/repack.py stage \\
    --image windows-11-desktop \\
    --work-dir <new-dir> --output-dir <new-dir> --cache-dir <dir>
  uv run --locked --script images/windows/repack.py import \\
    --image windows-11-desktop --output-dir <dir> \\
    --remote nas01 --project <project> --pool data --target lab01

`stage` needs root and loop devices, not KVM: distrobuilder's repack-windows
mounts the source ISO and the virtio ISO, injects the pinned driver set into
boot.wim and install.wim, and writes a new ISO. Windows itself is never booted
here; that happens on the cluster (see bake.py).

`--cache-dir` holds the large verified downloads between runs and is the only
place Microsoft media is allowed to live. Media never enters git, a CI
artifact, or a registry.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

import yaml

WINDOWS_DIR = Path(__file__).resolve().parent
IMAGES_DIR = WINDOWS_DIR.parent
PINS_PATH = WINDOWS_DIR / "pins.lock.yaml"
COMMON_DIR = WINDOWS_DIR / "common"
INSTANCE_PATH = COMMON_DIR / "instance.yaml"
IMAGE_NAMES = ("windows-11-desktop", "windows-server-2025")
GUEST_SCRIPTS = ("bootstrap.ps1", "finalize.ps1", "smoke.ps1", "deploy-firstlogon.ps1")
PASSWORD_TOKEN = "AUTOMATION_PASSWORD"


def load_build_module() -> Any:
    """Reuse the pinned download/compile helpers instead of duplicating them."""
    spec = importlib.util.spec_from_file_location("agentcompute_build", IMAGES_DIR / "build.py")
    if spec is None or spec.loader is None:
        raise Error(f"cannot load {IMAGES_DIR / 'build.py'}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class Error(RuntimeError):
    """Raised when validation or staging cannot continue."""


def load_yaml(path: Path) -> Any:
    try:
        return yaml.safe_load(path.read_text(encoding="utf-8"))
    except OSError as exc:
        raise Error(f"cannot read {path}: {exc}") from exc
    except yaml.YAMLError as exc:
        raise Error(f"invalid YAML in {path}: {exc}") from exc


def require_sha256(value: Any, name: str) -> str:
    if not isinstance(value, str) or len(value) != 64 or not all(
        char in "0123456789abcdef" for char in value.lower()
    ):
        raise Error(f"{name} is not a sha256 digest: {value!r}")
    return value.lower()


def load_pins() -> dict[str, Any]:
    pins = load_yaml(PINS_PATH)
    if not isinstance(pins, dict):
        raise Error(f"{PINS_PATH} must be a mapping")
    if pins.get("schema_version") != 1:
        raise Error(f"{PINS_PATH}: unsupported schema_version {pins.get('schema_version')!r}")

    media = pins.get("media")
    if not isinstance(media, dict) or set(media) != set(IMAGE_NAMES):
        raise Error(f"{PINS_PATH}: media must cover exactly {IMAGE_NAMES}")

    for name, entry in media.items():
        require_sha256(entry.get("sha256"), f"media.{name}.sha256")
        if not str(entry.get("url", "")).startswith("https://"):
            raise Error(f"media.{name}.url must be https")
        if entry.get("gate") not in ("cleared", "blocked"):
            raise Error(f"media.{name}.gate must be cleared or blocked")
        if entry.get("provenance") not in ("published", "derived", "measured"):
            raise Error(f"media.{name}.provenance is not a known value")
        if entry.get("provenance") == "published" and not entry.get("published_by"):
            raise Error(f"media.{name} claims published provenance without a publisher")
        if entry.get("gate") == "cleared" and entry.get("provenance") == "measured" \
                and not entry.get("gate_reason"):
            raise Error(
                f"media.{name} clears the gate on a measured digest without recording why"
            )

    virtio = pins.get("drivers", {}).get("virtio_win")
    if not isinstance(virtio, dict):
        raise Error(f"{PINS_PATH}: drivers.virtio_win is missing")
    require_sha256(virtio.get("iso_sha256"), "drivers.virtio_win.iso_sha256")
    chain = virtio.get("chain") or {}
    require_sha256(chain.get("sha256"), "drivers.virtio_win.chain.sha256")
    if not chain.get("md5") or not chain.get("md5_published_by"):
        raise Error("drivers.virtio_win.chain must record the published md5 and its source")

    payload = pins.get("guest_payload", {})
    for key in ("cua_driver", "ultravnc"):
        entry = payload.get(key)
        if not isinstance(entry, dict):
            raise Error(f"guest_payload.{key} is missing")
        require_sha256(entry.get("sha256"), f"guest_payload.{key}.sha256")
    servicing = payload.get("servicing", {})
    for package in servicing.get("packages") or []:
        require_sha256(package.get("sha256"), "guest_payload.servicing package sha256")
        if not package.get("url"):
            raise Error("every servicing package needs a url")

    return pins


def load_instance() -> dict[str, Any]:
    data = load_yaml(INSTANCE_PATH)
    if not isinstance(data, dict) or data.get("schema_version") != 1:
        raise Error(f"{INSTANCE_PATH}: unsupported or missing schema_version")
    images = data.get("images")
    if not isinstance(images, dict) or set(images) != set(IMAGE_NAMES):
        raise Error(f"{INSTANCE_PATH}: images must cover exactly {IMAGE_NAMES}")
    return data


def image_dir(image: str) -> Path:
    path = WINDOWS_DIR / image
    if not path.is_dir():
        raise Error(f"no answer-file directory for {image}: {path}")
    return path


def validate() -> dict[str, Any]:
    pins = load_pins()
    instance = load_instance()

    for script in GUEST_SCRIPTS:
        if not (COMMON_DIR / script).is_file():
            raise Error(f"missing guest script {COMMON_DIR / script}")

    for image in IMAGE_NAMES:
        directory = image_dir(image)
        for name in ("Autounattend.xml", "deploy-unattend.xml"):
            path = directory / name
            if not path.is_file():
                raise Error(f"missing {path}")
            text = path.read_text(encoding="utf-8")
            if "<AcceptEula>true</AcceptEula>" not in text and name == "Autounattend.xml":
                raise Error(f"{path} does not accept the evaluation terms; setup would stall")
            expected = pins["media"][image]["wim_image_name"]
            if name == "Autounattend.xml" and f"<Value>{expected}</Value>" not in text:
                raise Error(f"{path} does not select the pinned image {expected!r}")

        desktop = instance["images"][image]["desktop"]
        ultravnc_present = (directory / "ultravnc.ini").is_file()
        if desktop != ultravnc_present:
            raise Error(
                f"{image}: desktop={desktop} but ultravnc.ini present={ultravnc_present}"
            )

    return {"pins": pins, "instance": instance}


def md5_file(path: Path) -> str:
    hasher = hashlib.md5()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


def run(command: list[str], **kwargs: Any) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(command, check=True, text=True, **kwargs)
    except FileNotFoundError as exc:
        raise Error(f"missing command: {command[0]}") from exc
    except subprocess.CalledProcessError as exc:
        detail = (exc.stderr or exc.stdout or "").strip()
        raise Error(f"command failed: {' '.join(command)}\n{detail}") from exc


def fetch(build: Any, url: str, dest: Path, digest: str) -> dict[str, Any]:
    """Download once, then reuse: these inputs are gigabytes."""
    started = time.monotonic()
    reused = False
    if dest.is_file():
        actual = build.sha256_file(dest)
        if actual == digest:
            reused = True
        else:
            raise Error(f"{dest} exists with sha256 {actual}, expected {digest}")
    else:
        build.download(url, dest, digest)

    return {
        "file": dest.name,
        "url": url,
        "sha256": digest,
        "size": dest.stat().st_size,
        "reused": reused,
        "seconds": round(time.monotonic() - started, 3),
    }


def stage_virtio(build: Any, pins: dict[str, Any], cache: Path, work: Path) -> dict[str, Any]:
    virtio = pins["drivers"]["virtio_win"]
    chain = virtio["chain"]
    rpm = cache / chain["url"].rsplit("/", 1)[-1]
    record = fetch(build, chain["url"], rpm, chain["sha256"])

    actual_md5 = md5_file(rpm)
    if actual_md5 != chain["md5"]:
        raise Error(f"{rpm.name}: published md5 {chain['md5']} != {actual_md5}")

    iso = cache / virtio["iso_file"]
    if not iso.is_file():
        extract = work / "virtio-rpm"
        extract.mkdir(parents=True, exist_ok=True)
        # rpm2cpio | cpio keeps this to two host tools rather than adding an
        # rpm library dependency. The stream is binary, so it is piped rather
        # than buffered through text.
        with rpm.open("rb") as handle:
            unpack = subprocess.Popen(["rpm2cpio"], stdin=handle, stdout=subprocess.PIPE)
            assert unpack.stdout is not None
            extractor = subprocess.Popen(
                ["cpio", "-idmu", "--quiet"], stdin=unpack.stdout, cwd=extract
            )
            unpack.stdout.close()
            extractor_code = extractor.wait()
            unpack_code = unpack.wait()
        if unpack_code != 0 or extractor_code != 0:
            raise Error(
                f"rpm2cpio/cpio failed for {rpm.name}: "
                f"rpm2cpio={unpack_code} cpio={extractor_code}"
            )
        member = extract / chain["member"].lstrip("./")
        if not member.is_file():
            raise Error(f"{chain['member']} not found inside {rpm.name}")
        shutil.copy2(member, iso)

    actual = build.sha256_file(iso)
    if actual != virtio["iso_sha256"]:
        raise Error(f"{iso.name}: sha256 {actual} != pinned {virtio['iso_sha256']}")

    return {
        "rpm": record,
        "rpm_md5": actual_md5,
        "iso": {"file": iso.name, "sha256": actual, "size": iso.stat().st_size},
        "path": str(iso),
    }


def render_answer_file(source: Path, dest: Path, password: str) -> None:
    text = source.read_text(encoding="utf-8")
    if PASSWORD_TOKEN not in text:
        raise Error(f"{source} has no {PASSWORD_TOKEN} token to render")
    dest.write_text(text.replace(PASSWORD_TOKEN, password), encoding="utf-8")


def build_payload_tree(
    build: Any,
    pins: dict[str, Any],
    image: str,
    cache: Path,
    work: Path,
    output: Path,
    password: str,
    build_id: str,
) -> dict[str, Any]:
    started = time.monotonic()
    directory = image_dir(image)
    instance = load_instance()
    role = "desktop" if instance["images"][image]["desktop"] else "server-core"

    tree = work / "payload-tree"
    if tree.exists():
        shutil.rmtree(tree)
    agent_dir = tree / "agentcompute"
    payload_dir = agent_dir / "payload"
    payload_dir.mkdir(parents=True)

    # Windows Setup looks for Autounattend.xml in the root of removable media.
    render_answer_file(directory / "Autounattend.xml", tree / "Autounattend.xml", password)
    render_answer_file(
        directory / "deploy-unattend.xml", agent_dir / "deploy-unattend.xml", password
    )
    for script in GUEST_SCRIPTS:
        shutil.copy2(COMMON_DIR / script, agent_dir / script)

    guest_payload = pins["guest_payload"]
    config: dict[str, Any] = {
        "schema_version": 1,
        "image": image,
        "role": role,
        "build_id": build_id,
        "automation_user": "automation" if role == "desktop" else "Administrators",
        "state_dir": "C:\\ProgramData\\agentcompute",
        "driver_dir": guest_payload["cua_driver"]["install_dir"],
        "payload": {"servicing": []},
    }

    files: list[dict[str, Any]] = []
    if role == "desktop":
        driver = guest_payload["cua_driver"]
        archive = cache / driver["archive"]
        files.append(fetch(build, driver["url"], archive, driver["sha256"]))
        shutil.copy2(archive, payload_dir / archive.name)
        config["payload"]["cua_driver"] = {
            "file": archive.name,
            "sha256": driver["sha256"],
            "version": driver["version"],
            "archive_root": driver["archive_root"],
        }

        vnc = guest_payload["ultravnc"]
        setup = cache / vnc["file"]
        files.append(fetch(build, vnc["url"], setup, vnc["sha256"]))
        shutil.copy2(setup, payload_dir / setup.name)
        shutil.copy2(directory / "ultravnc.inf", payload_dir / "ultravnc.inf")
        shutil.copy2(directory / "ultravnc.ini", payload_dir / "ultravnc.ini")
        config["payload"]["ultravnc"] = {
            "file": setup.name,
            "sha256": vnc["sha256"],
            "version": vnc["version"],
            "service": vnc["service"],
            "port": vnc["port"],
        }

    for package in guest_payload["servicing"]["packages"] or []:
        target = cache / package["url"].rsplit("/", 1)[-1]
        files.append(fetch(build, package["url"], target, package["sha256"]))
        shutil.copy2(target, payload_dir / target.name)
        config["payload"]["servicing"].append(
            {"file": target.name, "sha256": package["sha256"], "kb": package.get("kb")}
        )

    (agent_dir / "config.json").write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")

    # The payload is grafted into the installer ISO rather than attached as a
    # second CD. Windows 11 25H2 Setup, given an Autounattend.xml on a
    # non-boot volume, decided it was an interrupted upgrade and stopped on
    # "It looks like you started an upgrade and booted from installation
    # media" - observed live on the first bake attempt. One boot volume
    # carrying both the answer file and the payload removes that ambiguity and
    # the drive-letter guessing with it.
    return {
        "tree": str(tree),
        "role": role,
        "downloads": files,
        "seconds": round(time.monotonic() - started, 3),
        "config": config,
    }


def repack_installer(
    build: Any,
    pins: dict[str, Any],
    image: str,
    cache: Path,
    work: Path,
    output: Path,
    virtio_iso: Path,
    distrobuilder: Path,
    payload_tree: Path,
) -> dict[str, Any]:
    media = pins["media"][image]
    source = cache / f"{image}-source.iso"
    record = fetch(build, media["url"], source, media["sha256"])

    driver_iso = work / f"{image}-drivers.iso"
    token = pins["tools"]["distrobuilder"]["windows_version_token"][image]
    arch = pins["tools"]["distrobuilder"]["windows_arch_token"]
    started = time.monotonic()
    run([
        str(distrobuilder), "repack-windows", str(source), str(driver_iso),
        "--drivers", str(virtio_iso),
        "--windows-version", token,
        "--windows-arch", arch,
        "--cache-dir", str(work / "distrobuilder-cache"),
    ], stdout=None, stderr=None)

    if not driver_iso.is_file():
        raise Error("repack-windows did not produce an installer ISO")

    repack_seconds = round(time.monotonic() - started, 3)
    target = output / f"{image}-installer.iso"
    noprompt = rebuild_without_boot_prompt(driver_iso, target, work, payload_tree)

    return {
        "source": record,
        "iso": str(target),
        "sha256": build.sha256_file(target),
        "size": target.stat().st_size,
        "windows_version": token,
        "windows_arch": arch,
        "seconds": repack_seconds,
        "driver_injected_iso": {
            "path": str(driver_iso),
            "sha256": build.sha256_file(driver_iso),
            "size": driver_iso.stat().st_size,
        },
        "boot_prompt_removed": noprompt,
    }


def rebuild_without_boot_prompt(
    source: Path, target: Path, work: Path, payload_tree: Path
) -> dict[str, Any]:
    """Re-emit the ISO with Microsoft's no-prompt UEFI boot image.

    distrobuilder builds its ISO with `efi/microsoft/boot/efisys.bin`, which is
    the variant that asks the user to "press any key to boot from CD". In an
    unattended VM bake nobody presses a key, the prompt times out, and the
    firmware falls through to the next boot entry: observed live as
    `failed to start Boot0002 "UEFI QEMU QEMU CD-ROM": Time out` followed by
    PXE attempts. Microsoft ships `efisys_noprompt.bin` on the same media for
    exactly this case, so the driver-injected tree is re-emitted with it. The
    El Torito arguments otherwise match distrobuilder's genisoimage branch.
    """
    mount = work / "repack-mount"
    mount.mkdir(parents=True, exist_ok=True)
    started = time.monotonic()
    run(["mount", "-t", "udf", "-o", "loop,ro", str(source), str(mount)])
    try:
        boot_image = mount / "efi/microsoft/boot/efisys_noprompt.bin"
        if not boot_image.is_file():
            raise Error(f"{source.name} has no efi/microsoft/boot/efisys_noprompt.bin")
        # Graft points keep this cheap: the multi-gigabyte installer tree is
        # read straight from the mounted source, and only the answer file and
        # the provisioning payload are added from the work directory.
        run([
            "genisoimage", "-quiet", "-l", "-iso-level", "4", "-no-emul-boot",
            "-b", "boot/etfsboot.com", "-boot-load-seg", "0", "-boot-load-size", "8",
            "-eltorito-alt-boot", "--allow-limited-size", "-no-emul-boot",
            "-e", "efi/microsoft/boot/efisys_noprompt.bin", "-boot-load-size", "1", "-udf",
            "-graft-points", "-o", str(target),
            f"/={mount}",
            f"/Autounattend.xml={payload_tree / 'Autounattend.xml'}",
            f"/agentcompute/={payload_tree / 'agentcompute'}",
        ])
    finally:
        subprocess.run(["umount", str(mount)], check=False, capture_output=True, text=True)

    if not target.is_file():
        raise Error("no-prompt ISO rebuild produced no file")

    return {
        "boot_image": "efi/microsoft/boot/efisys_noprompt.bin",
        "seconds": round(time.monotonic() - started, 3),
    }


def stage(args: argparse.Namespace) -> int:
    build = load_build_module()
    loaded = validate()
    pins = loaded["pins"]
    media = pins["media"][args.image]
    if media["gate"] != "cleared":
        raise Error(
            f"{args.image} media gate is {media['gate']}: {media.get('gate_reason', '')}"
        )

    if os.geteuid() != 0:
        raise Error("stage needs root: repack-windows mounts ISOs and uses loop devices")

    for tool in ("hivexregedit", "rsync", "wimlib-imagex", "genisoimage", "rpm2cpio", "cpio"):
        if shutil.which(tool) is None:
            raise Error(f"missing host tool required for staging: {tool}")

    cache = Path(args.cache_dir).expanduser().resolve()
    cache.mkdir(parents=True, exist_ok=True)
    work = build.require_new_dir(Path(args.work_dir), "--work-dir")
    output = build.require_new_dir(Path(args.output_dir), "--output-dir")
    started = time.monotonic()

    distrobuilder: Path
    compile_seconds = 0.0
    if args.distrobuilder:
        distrobuilder = Path(args.distrobuilder).expanduser().resolve()
        if not distrobuilder.is_file():
            raise Error(f"--distrobuilder {distrobuilder} is not a file")
    else:
        image_pins = build.load_pins()
        downloads = work / "downloads"
        downloads.mkdir(mode=0o700)
        for key in ("go", "distrobuilder"):
            entry = image_pins[key]
            build.download(entry["url"], downloads / entry["url"].rsplit("/", 1)[-1], entry["sha256"])
        distrobuilder, compile_seconds, _ = build.compile_distrobuilder(image_pins, work, downloads)

    version = build.first_line([str(distrobuilder), "--version"])
    expected = pins["tools"]["distrobuilder"]["version"]
    if expected not in version:
        raise Error(f"distrobuilder reports {version!r}, expected {expected}")

    virtio = stage_virtio(build, pins, cache, work)
    payload = build_payload_tree(
        build, pins, args.image, cache, work, output,
        args.automation_password, args.build_id or f"{args.image}-local",
    )
    installer = repack_installer(
        build, pins, args.image, cache, work, output, Path(virtio["path"]), distrobuilder,
        Path(payload["tree"]),
    )

    metrics = {
        "schema_version": 1,
        "image": args.image,
        "build_id": payload["config"]["build_id"],
        "distrobuilder": {"version": version, "compile_seconds": compile_seconds},
        "virtio": virtio,
        "installer": installer,
        "payload": {key: value for key, value in payload.items() if key != "config"},
        "media_provenance": {
            "provenance": media["provenance"],
            "published_by": media.get("published_by"),
            "gate_reason": media.get("gate_reason"),
        },
        "automation_password_set": bool(args.automation_password),
        "scratch_high_water_bytes": build.scratch_bytes(work),
        "peak_rss_kib": build.rss_kib(),
        "total_seconds": round(time.monotonic() - started, 3),
        "host": os.uname().nodename,
    }
    (output / "metrics.json").write_text(json.dumps(metrics, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(metrics, indent=2))
    return 0


def volume_exists(remote: str, project: str, pool: str, name: str) -> bool:
    result = subprocess.run(
        ["incus", "storage", "volume", "list", f"{remote}:{pool}",
         "--project", project, "--format", "csv", "-c", "n"],
        capture_output=True, text=True, check=False,
    )
    wanted = {f"custom/{name}", name}
    return any(line.strip() in wanted for line in result.stdout.splitlines())


def import_volumes(args: argparse.Namespace) -> int:
    output = Path(args.output_dir).expanduser().resolve()
    results = []
    for suffix in ("installer",):
        iso = output / f"{args.image}-{suffix}.iso"
        if not iso.is_file():
            raise Error(f"missing staged ISO {iso}")
        name = f"{args.volume_prefix}-{suffix}"
        if volume_exists(args.remote, args.project, args.pool, name):
            if not args.replace:
                raise Error(f"volume {name} already exists; pass --replace to overwrite")
            run(["incus", "storage", "volume", "delete", f"{args.remote}:{args.pool}", name,
                 "--project", args.project])

        command = [
            "incus", "storage", "volume", "import", f"{args.remote}:{args.pool}", str(iso),
            name, "--type=iso", "--project", args.project,
        ]
        if args.target:
            command += ["--target", args.target]
        run(command, stdout=None, stderr=None)
        results.append({"volume": name, "source": str(iso), "size": iso.stat().st_size})

    print(json.dumps({"volumes": results, "project": args.project, "pool": args.pool}, indent=2))
    return 0


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("validate", help="check the lock file, answer files and guest scripts")

    stage_cmd = sub.add_parser("stage", help="repack the installer and build the payload ISO")
    stage_cmd.add_argument("--image", choices=IMAGE_NAMES, required=True)
    stage_cmd.add_argument("--work-dir", required=True, help="must not exist yet")
    stage_cmd.add_argument("--output-dir", required=True, help="must not exist yet")
    stage_cmd.add_argument("--cache-dir", required=True, help="verified download cache")
    stage_cmd.add_argument("--distrobuilder", help="pre-built pinned distrobuilder binary")
    stage_cmd.add_argument("--build-id", help="identifier recorded in the guest payload")
    stage_cmd.add_argument(
        "--automation-password", default="",
        help="auto-logon password; empty (the default) bakes no reusable secret",
    )

    import_cmd = sub.add_parser("import", help="import the staged ISOs as Incus volumes")
    import_cmd.add_argument("--image", choices=IMAGE_NAMES, required=True)
    import_cmd.add_argument("--output-dir", required=True)
    import_cmd.add_argument("--remote", default="local")
    import_cmd.add_argument("--project", required=True)
    import_cmd.add_argument("--pool", default="data")
    import_cmd.add_argument("--target", help="cluster member for the volume")
    import_cmd.add_argument("--volume-prefix", required=True)
    import_cmd.add_argument("--replace", action="store_true")

    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    try:
        if args.command == "validate":
            validate()
            print("ok")
            return 0
        if args.command == "stage":
            return stage(args)
        return import_volumes(args)
    except Error as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
