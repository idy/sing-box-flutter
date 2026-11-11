"""Windows build cache regressions without invoking Go or downloading inputs."""
import json
import sys
from unittest import mock

sys.dont_write_bytecode = True
from test_windows_core import WindowsFixture
import build_core
from native_common import artifact_hashes, write_json


class WindowsBuildReview(WindowsFixture):
    def test_cached_rebuild_repairs_windows_arch_and_reuses_immutable_bundle(self):
        entry, _, _ = self.published()
        artifact = build_core.host_path(self.host, entry["path"])
        metadata_path = build_core.host_path(self.host, entry["metadata"])
        original_metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
        original_hashes = artifact_hashes(artifact)
        original_files = {path.name: (path.read_bytes(), path.stat().st_mtime_ns)
                          for path in artifact.iterdir()}
        arguments = ("--windows-arch", "amd64", "--print-artifact-pair")
        original_recipe = self.command(*arguments)
        self.assertEqual(original_recipe.returncode, 0, original_recipe.stderr)

        for corrupt_arch in ("arm64", None, ["amd64"]):
            with self.subTest(windows_arch=corrupt_arch):
                corrupt = {**original_metadata, "windows_arch": corrupt_arch}
                if corrupt_arch is None:
                    del corrupt["windows_arch"]
                write_json(metadata_path, corrupt)
                rejected = self.command(*arguments)
                self.assertEqual(rejected.returncode, 1, rejected.stdout)
                self.assertIn("stale or invalid", rejected.stderr)

                # Rebuild publication must repair metadata in place on this cache hit.
                with mock.patch("build_core.shutil.copytree", side_effect=AssertionError("bundle copied")):
                    rebuilt_entry, rebuilt_receipt, _ = self.published()
                self.assertEqual(rebuilt_entry, entry)
                self.assertEqual(rebuilt_receipt["artifacts"]["windows"], entry)
                self.assertEqual(json.loads(metadata_path.read_text(encoding="utf-8")), original_metadata)
                self.assertEqual(artifact_hashes(artifact), original_hashes)
                self.assertEqual({path.name: (path.read_bytes(), path.stat().st_mtime_ns)
                                  for path in artifact.iterdir()}, original_files)
                repaired_recipe = self.command(*arguments)
                self.assertEqual(repaired_recipe.returncode, 0, repaired_recipe.stderr)
                self.assertEqual(repaired_recipe.stdout, original_recipe.stdout)
