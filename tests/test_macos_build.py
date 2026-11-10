"""macOS mode identity, native SDK targets and host build guards."""
import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
from build_core import (bind_command, build_identity, canonical_hash, canonical_macos,
                        invalidate_pods, load_host_build_request)
from core_features import canonicalize, resolve_features
from core_recipe import verify_current_recipe
from native_common import BuildError, load_pins, sha256


class MacOSBuild(unittest.TestCase):
    def test_mode_is_strict_and_default_is_ne(self):
        self.assertEqual(canonical_macos({}), {"mode": "ne"})
        for invalid in (None, [], "ne", {"mode": None}, {"mode": "tun"},
                        {"mode": ["ne", "user_tun"]}, {"mode": "ne", "helper": True}):
            with self.subTest(invalid=invalid), self.assertRaises(BuildError):
                canonical_macos(invalid)

    def test_modes_have_distinct_recipe_fingerprints_and_are_verified(self):
        selection = canonicalize({})
        pins = load_pins()
        hashes = set()
        with tempfile.TemporaryDirectory() as directory:
            project = Path(directory)
            (project / "pubspec.yaml").write_text("name: host\n")
            resolved = resolve_features(selection, "macos")
            for mode in ("ne", "user_tun"):
                macos = {"mode": mode}
                identity = build_identity(selection, [], pins, macos=macos)
                fingerprint = canonical_hash(identity)
                hashes.add(fingerprint)
                receipt = {"schema": 1, "version": pins["sing_box_version"], "macos": macos,
                           "fingerprint": fingerprint, "requested_features": selection,
                           "features": resolved["features"],
                           "pubspec_sha256": sha256(project / "pubspec.yaml")}
                metadata = {"schema": 1, "mode": "selectable-core", "target": "macos",
                            "identity": identity, "fingerprint": fingerprint,
                            "features": resolved["features"], "tags": resolved["tags"]}
                verify_current_recipe(project, receipt, metadata, target="macos")
                changed = copy.deepcopy(receipt)
                changed["macos"]["mode"] = "user_tun" if mode == "ne" else "ne"
                with self.assertRaises(BuildError):
                    verify_current_recipe(project, changed, metadata, target="macos")
            self.assertEqual(len(hashes), 2)

    def test_macos_request_is_bound_to_the_manifest_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            project = Path(directory)
            pubspec = project / "pubspec.yaml"
            pubspec.write_text("sing_box_flutter:\n  macos: {mode: user_tun}\n")
            document = {"schema": 1, "pubspec": {"path": str(pubspec.resolve()),
                        "sha256": sha256(pubspec)}, "features": {}, "macos": {"mode": "user_tun"}}
            request = project / "request.json"
            request.write_text(json.dumps(document))
            selection, digest, macos = load_host_build_request(request, project)
            self.assertEqual(selection, canonicalize({}))
            self.assertEqual(digest, sha256(pubspec))
            self.assertEqual(macos, {"mode": "user_tun"})
            pubspec.write_text("sing_box_flutter:\n  macos: {mode: ne}\n")
            with self.assertRaises(BuildError):
                load_host_build_request(request, project)

    def test_bind_targets_both_mac_architectures_without_ios_low_memory(self):
        resolved = resolve_features(canonicalize({}), "macos")
        command = bind_command({"GOBIN": "/tools"}, Path("Libbox.xcframework"), "macos",
                               resolved["tags"], "a" * 64, load_pins(), [], "ios")
        self.assertEqual(command[command.index("-target") + 1], "macos/arm64,macos/amd64")
        self.assertIn("-macosversion=12.0", command)
        self.assertNotIn("with_low_memory", resolved["tags"])
        self.assertNotIn("-iosversion=15.0", command)
        with self.assertRaises(ValueError):
            resolve_features(canonicalize({"outbounds": ["naive"]}), "macos")

    def test_macos_pods_are_invalidated_independently(self):
        with tempfile.TemporaryDirectory() as directory:
            project = Path(directory)
            for platform in ("ios", "macos"):
                pods = project / platform / "Pods"
                pods.mkdir(parents=True)
                (pods / "Manifest.lock").write_text("generated")
                (pods.parent / "Podfile.lock").write_text("locked")
            invalidate_pods(project, "macos")
            self.assertFalse((project / "macos/Pods/Manifest.lock").exists())
            self.assertTrue((project / "ios/Pods/Manifest.lock").exists())
            self.assertEqual((project / "macos/Podfile.lock").read_text(), "locked")


if __name__ == "__main__":
    unittest.main()
