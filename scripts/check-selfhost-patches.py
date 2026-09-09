#!/usr/bin/env python3
"""Read-only integration check; never selects a deployment or starts a service.

The manifest in this checkout is the reviewed contract. --target selects the
tree to audit against it; --current additionally rejects losing any commit from
the currently deployed build. The owner need not choose either SHA: the update
operator obtains them from deployed build metadata and the reviewed PR.
"""

import argparse
import json
from pathlib import Path
import re
import subprocess
import sys


def git(repo, *args):
    return subprocess.run(
        ["git", "-C", str(repo), *args], capture_output=True, text=True, check=False
    )


def resolve(repo, value):
    result = git(repo, "rev-parse", "--verify", "--end-of-options", value + "^{commit}")
    if result.returncode:
        raise ValueError(f"commit unavailable: {value}; fetch full history first")
    return result.stdout.strip()


def check(repo, manifest, target, current=None):
    target = resolve(repo, target)
    errors = []
    if manifest.get("schema_version") != 1 or not manifest.get("patches"):
        raise ValueError("unsupported or empty selfhost patch manifest")
    if current:
        current = resolve(repo, current)
        missing = git(repo, "rev-list", "--oneline", current, "--not", target)
        if missing.returncode:
            raise ValueError(missing.stderr.strip())
        if missing.stdout.strip():
            errors.append("deployed commits missing from target:\n" + missing.stdout.strip())
    ids = set()
    for patch in manifest["patches"]:
        patch_id = patch["id"]
        if not patch_id or patch_id in ids:
            raise ValueError("patch ids must be nonempty and unique")
        ids.add(patch_id)
        commit = patch["introduced_by"]
        if not re.fullmatch(r"[0-9a-f]{40}", commit):
            raise ValueError(f"{patch_id}: introduced_by must be a full commit id")
        if git(repo, "merge-base", "--is-ancestor", commit, target).returncode:
            errors.append(f"{patch_id}: missing accepted commit {commit}")
        for path in patch.get("required_files", []):
            if git(repo, "cat-file", "-e", f"{target}:{path}").returncode:
                errors.append(f"{patch_id}: missing required file {path}")
        for path in patch.get("absent_files", []):
            if not git(repo, "cat-file", "-e", f"{target}:{path}").returncode:
                errors.append(f"{patch_id}: retired file restored: {path}")
        for test in patch["required_tests"]:
            if not re.fullmatch(r"Test[A-Za-z0-9_]+", test):
                raise ValueError(f"{patch_id}: invalid test name {test}")
            pattern = "^func " + test + r"\(t \*testing\.T\)"
            result = git(repo, "grep", "-q", "-E", pattern, target, "--", "server/**/*.go")
            if result.returncode:
                errors.append(f"{patch_id}: missing regression test {test}")
    return target, errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", default="HEAD")
    parser.add_argument("--current", help="current deployed commit, from build metadata")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    try:
        manifest = json.loads((root / "selfhost/patches.json").read_text())
        target, errors = check(root, manifest, args.target, args.current)
    except (KeyError, TypeError, ValueError, OSError) as error:
        print(f"selfhost integrity ERROR: {error}", file=sys.stderr)
        return 2
    if errors:
        print("selfhost integrity FAILED:\n- " + "\n- ".join(errors), file=sys.stderr)
        return 1
    print(f"selfhost integrity PASS: {target}; {len(manifest['patches'])} accepted patches")
    print("This is a static gate, not a deployment or a substitute for behavioral tests.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
