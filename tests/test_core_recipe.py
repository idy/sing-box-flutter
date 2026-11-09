"""Selected artifacts must match the currently resolved plugin, not just themselves."""
import copy
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
from build_core import build_identity, canonical_hash
from core_features import canonicalize, resolve_features
from core_recipe import verify_current_recipe
from native_common import BuildError, ROOT, load_pins, sha256


class CurrentRecipe(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        base = Path(self.directory.name)
        self.host = base / "host"
        self.host.mkdir()
        (self.host / "pubspec.yaml").write_text("name: recipe_fixture\n")
        self.plugin = base / "plugin"
        (self.plugin / "tool").mkdir(parents=True)
        shutil.copyfile(ROOT / "native-core.json", self.plugin / "native-core.json")
        for name in ("tailscale-go.json", "build_core.py", "core_features.py", "build_libbox.py",
                     "native_common.py", "wgc_overlay.py", "source_inputs.py"):
            shutil.copyfile(ROOT / "tool" / name, self.plugin / "tool" / name)
        (self.plugin / "native").mkdir()
        shutil.copytree(ROOT / "native/wgc", self.plugin / "native/wgc")
        (self.plugin / "native/mobile").mkdir()
        (self.plugin / "native/mobile/runtime.go").write_text("package libbox\n// recipe A\n")
        selection = canonicalize({})
        pins = load_pins(self.plugin)
        identity = build_identity(selection, [], pins, self.plugin)
        fingerprint = canonical_hash(identity)
        resolved = resolve_features(selection, "android")
        self.receipt = {"schema": 1, "version": pins["sing_box_version"], "fingerprint": fingerprint,
                        "requested_features": selection, "features": resolved["features"],
                        "pubspec_sha256": sha256(self.host / "pubspec.yaml")}
        self.metadata = {"schema": 1, "mode": "selectable-core", "target": "android",
                         "identity": identity, "fingerprint": fingerprint,
                         "features": resolved["features"], "tags": resolved["tags"]}

    def verify(self, receipt=None, metadata=None):
        verify_current_recipe(self.host, self.receipt if receipt is None else receipt,
                              self.metadata if metadata is None else metadata,
                              target="android", root=self.plugin)

    def test_current_source_matches(self):
        self.verify()

    def test_cli_works_with_isolated_python(self):
        from native_common import write_json
        selection = canonicalize({})
        pins = load_pins()
        identity = build_identity(selection, [], pins)
        fingerprint = canonical_hash(identity)
        resolved = resolve_features(selection, "android")
        receipt = {"schema": 1, "version": pins["sing_box_version"], "fingerprint": fingerprint,
                   "requested_features": selection, "features": resolved["features"],
                   "pubspec_sha256": sha256(self.host / "pubspec.yaml"),
                   "artifacts": {"android": {"metadata": ".dart_tool/sing_box_flutter/metadata.json"}}}
        metadata = {"schema": 1, "mode": "selectable-core", "target": "android", "identity": identity,
                    "fingerprint": fingerprint, "features": resolved["features"], "tags": resolved["tags"]}
        cache = self.host / ".dart_tool/sing_box_flutter"
        write_json(cache / "native.json", receipt)
        write_json(cache / "metadata.json", metadata)
        result = subprocess.run([sys.executable, "-I", str(ROOT / "tool/core_recipe.py"),
                                 "--project-dir", str(self.host), "--target", "android"],
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_runtime_source_edit_is_stale_without_pubspec_change(self):
        (self.plugin / "native/mobile/runtime.go").write_text("package libbox\n// recipe B\n")
        with self.assertRaisesRegex(BuildError, "stale"):
            self.verify()

    def test_added_source_is_stale(self):
        (self.plugin / "native/mobile/extra.go").write_text("package libbox\n")
        with self.assertRaises(BuildError):
            self.verify()

    def test_tool_and_pin_edits_are_stale(self):
        for relative in ("tool/core_features.py", "native/wgc/transport.go"):
            path = self.plugin / relative
            original = path.read_bytes()
            path.write_bytes(original + b"\n// changed recipe\n")
            with self.subTest(path=relative), self.assertRaises(BuildError):
                self.verify()
            path.write_bytes(original)
        pins = json.loads((self.plugin / "native-core.json").read_text())
        pins["gomobile_version"] = "v0.1.13"
        (self.plugin / "native-core.json").write_text(json.dumps(pins))
        with self.assertRaises(BuildError):
            self.verify()

    def test_pubspec_change_and_self_consistent_old_fingerprint_are_rejected(self):
        (self.host / "pubspec.yaml").write_text("name: changed_host\n")
        with self.assertRaises(BuildError):
            self.verify()

    def test_fingerprint_feature_and_target_mismatch(self):
        for field, value in (("fingerprint", "b" * 64), ("features", {}),
                             ("tags", []), ("target", "apple"), ("mode", "other")):
            metadata = copy.deepcopy(self.metadata)
            metadata[field] = value
            with self.subTest(field=field), self.assertRaises(BuildError):
                self.verify(metadata=metadata)
        receipt = copy.deepcopy(self.receipt)
        receipt["requested_features"]["endpoints"] = ["tailscale"]
        with self.assertRaises(BuildError):
            self.verify(receipt=receipt)


if __name__ == "__main__":
    unittest.main()
