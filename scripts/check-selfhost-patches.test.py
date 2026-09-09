#!/usr/bin/env python3
"""Exercise the integration gate against disposable git histories."""

import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location(
    "integrity", Path(__file__).with_name("check-selfhost-patches.py")
)
integrity = importlib.util.module_from_spec(spec)
spec.loader.exec_module(integrity)


class IntegrityTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="selfhost-integrity-")
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        self.git("init", "-q")
        self.file = self.repo / "server/internal/feature_test.go"
        self.file.parent.mkdir(parents=True)
        self.file.write_text("package internal\n")
        self.base = self.commit("base")
        self.file.write_text("package internal\nfunc TestRetained(t *testing.T) {}\n")
        self.patch = self.commit("accepted capability")
        self.manifest = {
            "schema_version": 1,
            "patches": [{
                "id": "retained",
                "introduced_by": self.patch,
                "required_files": ["server/internal/feature_test.go"],
                "absent_files": ["retired.go"],
                "required_tests": ["TestRetained"],
            }],
        }

    def git(self, *args):
        return subprocess.check_output([
            "git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
            "-c", "commit.gpgsign=false", "-C", str(self.repo), *args,
        ], text=True).strip()

    def commit(self, message, paths=("server/internal/feature_test.go",)):
        self.git("add", "--", *paths)
        self.git("commit", "-q", "-m", message, "--", *paths)
        return self.git("rev-parse", "HEAD")

    def errors(self, target="HEAD", current=None):
        return integrity.check(self.repo, self.manifest, target, current)[1]

    def test_retained_patch_passes(self):
        self.assertEqual(self.errors(current=self.base), [])

    def test_omitted_feature_branch_fails(self):
        self.assertTrue(any("missing accepted commit" in e for e in self.errors(self.base)))

    def test_previously_deployed_unlisted_commit_cannot_disappear(self):
        self.file.write_text(self.file.read_text() + "// another deployed change\n")
        deployed = self.commit("deployed but not yet inventoried")
        errors = self.errors(self.patch, deployed)
        self.assertTrue(any("deployed commits missing" in e for e in errors))

    def test_ancestry_alone_does_not_allow_deleted_behavior_test(self):
        self.file.write_text("package internal\n")
        self.commit("accidentally drop regression test")
        self.assertTrue(any("missing regression test" in e for e in self.errors()))

    def test_reintroducing_retired_implementation_fails(self):
        (self.repo / "retired.go").write_text("package retired\n")
        self.commit("restore retired path", ("retired.go",))
        self.assertTrue(any("retired file restored" in e for e in self.errors()))

    def test_unknown_current_commit_fails_closed(self):
        with self.assertRaisesRegex(ValueError, "commit unavailable"):
            self.errors(current="0" * 40)

    def test_empty_manifest_fails_closed(self):
        self.manifest["patches"] = []
        with self.assertRaisesRegex(ValueError, "empty"):
            self.errors()


if __name__ == "__main__":
    unittest.main()
