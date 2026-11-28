#!/usr/bin/env python3
"""Package a signed macOS arm64 app for the offline Desktop installer."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import plistlib
import re
import stat
import subprocess
import tarfile


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def tree_hash(app):
    # Schema 1 hashes the relative names, modes, bytes and symlink targets.
    digest = hashlib.sha256()
    root = app.resolve()
    for path in sorted(app.rglob("*")):
        mode = stat.S_IMODE(path.lstat().st_mode)
        if path.is_symlink():
            require(path.resolve().is_relative_to(root), f"Escaping symlink: {path}")
            value = ["link", os.readlink(path)]
        elif path.is_file():
            value = ["file", mode, sha256(path)]
        elif path.is_dir():
            value = ["directory", mode]
        else:
            raise ValueError(f"Unsupported application member: {path}")
        digest.update(json.dumps([path.relative_to(app).as_posix(), value]).encode())
        digest.update(b"\n")
    return digest.hexdigest()


def output(*argv):
    return subprocess.check_output(argv, text=True).strip()


def pack(app, directory, commit):
    require(re.fullmatch(r"[a-f0-9]{40}", commit), "A full source commit is required")
    require(app.is_dir() and not app.is_symlink(), "A real application directory is required")
    subprocess.run(["/usr/bin/codesign", "--verify", "--deep", "--strict", app], check=True)
    resources = app / "Contents/Resources/app.asar.unpacked/resources"
    identity = json.loads((resources / "desktop-release.json").read_text())
    require(identity["commit"] == commit and identity["source_clean"] is True,
            "Desktop was not built from the expected clean commit")
    require((identity["platform"], identity["arch"]) == ("darwin", "arm64"),
            "Expected a darwin arm64 Desktop")
    require(identity["update_mode"] in ("manual", "coordinated"), "Unrecognized update mode")
    with (app / "Contents/Info.plist").open("rb") as stream:
        info = plistlib.load(stream)
    executable = info["CFBundleExecutable"]
    require(executable and Path(executable).name == executable, "Invalid Desktop executable")
    cli = resources / "bin/multica"
    require(cli.is_file() and not cli.is_symlink(), "Missing bundled CLI")
    for binary in (app / "Contents/MacOS" / executable, cli):
        require(output("/usr/bin/lipo", "-archs", binary) == "arm64", f"Incorrect architecture: {binary}")
    cli_info = json.loads(output(cli, "version", "--output", "json"))
    require(cli_info["build_commit"] == commit, "Bundled CLI has a different commit")
    version = info["CFBundleShortVersionString"]
    require(cli_info["version"].removeprefix("v") == version, "Desktop and CLI versions differ")

    directory.mkdir()
    archive = directory / "Multica.tar.gz"
    digest = tree_hash(app)
    with tarfile.open(archive, "w:gz") as stream:
        stream.add(app, arcname="Multica.app")
    require(tree_hash(app) == digest, "Application changed while packing")
    manifest = {
        **identity,
        "schema_version": 1,
        "version": version,
        "cli_sha256": sha256(cli),
        "tree_sha256": digest,
        "archive_bytes": archive.stat().st_size,
        "archive_sha256": sha256(archive),
    }
    (directory / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--app", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--commit", required=True)
    args = parser.parse_args()
    pack(args.app, args.output, args.commit)
