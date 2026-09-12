#!/usr/bin/env python3
"""Install the pinned Incus CLI and stage the private bake job's TLS identity."""

import hashlib
import os
from pathlib import Path
import subprocess
import sys
import urllib.request

import yaml


def main() -> None:
    conf, bin_dir = map(Path, sys.argv[1:])
    required = ("INCUS_CLIENT_CERT", "INCUS_CLIENT_KEY", "INCUS_SERVER_CERT")
    if any(not os.environ.get(name, "").strip() for name in required):
        raise ValueError("all three Incus TLS credentials are required")
    os.umask(0o077)
    (conf / "servercerts").mkdir(parents=True, exist_ok=False)
    for name, path in zip(required, (conf / "client.crt", conf / "client.key", conf / "servercerts/nas01.crt")):
        path.write_text(os.environ[name].strip() + "\n")
    (conf / "config.yml").write_text(yaml.safe_dump({
        "default-remote": "nas01",
        "remotes": {"nas01": {"addr": "https://10.10.10.14:8443", "auth_type": "tls",
                              "project": "image-build", "protocol": "incus", "public": False}},
    }))
    pin = yaml.safe_load(Path("images/pins.yaml").read_text())["incus"]
    if not pin["url"].startswith("https://github.com/lxc/incus/releases/download/"):
        raise ValueError("unexpected Incus release source")
    bin_dir.mkdir(parents=True, exist_ok=True)
    binary = bin_dir / "incus"
    hasher = hashlib.sha256()
    with urllib.request.urlopen(pin["url"], timeout=120) as response, binary.open("xb") as output:
        if not response.url.startswith("https://"):
            raise ValueError("Incus download redirected away from HTTPS")
        while block := response.read(1024 * 1024):
            hasher.update(block)
            output.write(block)
    if hasher.hexdigest() != pin["sha256"]:
        binary.unlink()
        raise ValueError("Incus release checksum mismatch")
    binary.chmod(0o755)
    version = subprocess.check_output([str(binary), "--version"], text=True).strip()
    if version != pin["version"]:
        raise ValueError("Incus version differs from pin")
    subprocess.run([str(binary), "list", "nas01:", "--project", "image-build", "--format=json"],
                   env={**os.environ, "INCUS_CONF": str(conf)}, check=True, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
