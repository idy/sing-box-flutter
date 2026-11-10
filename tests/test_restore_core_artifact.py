"""Offline CI handoff of selected Android cores, with source-build fallback."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import unittest
from unittest import mock
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
from build_core import (ANDROID_TARGETS, RECEIPT, build_identity, canonical_hash,
                        publish_artifact, update_receipt)
from core_features import canonicalize, generate_registry, resolve_features
from native_common import BuildError, ROOT, artifact_hashes, legal_files, load_pins, sha256, write_json
from restore_core_artifact import (MANIFEST, RestoreMiss, bundle, main, restore_artifact,
                                   write_manifest)


class RestoreCoreArtifact(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.producer = self.base / "producer"
        self.host = self.base / "host"
        pubspec = """name: example
sing_box_flutter:
  core:
    inbounds: [tun]
    outbounds: [direct]
    endpoints: [wgc]
    dns: [udp, tcp, fakeip]
"""
        for project in (self.producer, self.host):
            project.mkdir()
            (project / "pubspec.yaml").write_text(pubspec)
        self.selection = canonicalize({"inbounds": ["tun"], "outbounds": ["direct"],
                                       "endpoints": ["wgc"], "dns": ["udp", "tcp", "fakeip"]})
        effective = resolve_features(self.selection, "android")
        identity = build_identity(self.selection, [], load_pins())
        self.capabilities = {"schema": 1, "version": "1.14.4", "fingerprint": canonical_hash(identity),
                             "features": effective["features"]}
        self.identity = identity
        self.metadata = {"schema": 1, "mode": "selectable-core", "target": "android", "identity": identity,
                         "fingerprint": self.capabilities["fingerprint"],
                         "features": effective["features"], "tags": effective["tags"],
                         "registry_sha256": hashlib.sha256(
                             generate_registry(effective["features"]).encode()).hexdigest()}
        self.receipt_path = self.host / RECEIPT
        self.receipt_path.parent.mkdir(parents=True)
        self.receipt_path.write_bytes(b"previous host receipt\n")
        self.previous_receipt = self.receipt_path.read_bytes()
        self.export()

    def export(self, abis=tuple(ANDROID_TARGETS), *, validate=True):
        shutil.rmtree(self.producer / ".dart_tool", ignore_errors=True)
        aar = self.base / "libbox.aar"
        with zipfile.ZipFile(aar, "w") as archive:
            archive.writestr("AndroidManifest.xml", "manifest")
            archive.writestr("classes.jar", "classes")
            for abi in abis:
                archive.writestr(f"jni/{abi}/libbox.so", f"native {abi}")
        upstream = self.base / "LICENSE.sing-box"
        wgc = self.base / "LICENSE.wgc"
        upstream.write_text("upstream license\n")
        wgc.write_text("WGC license\n")
        notices = {**legal_files(ROOT), "NOTICE.mobile-core": ROOT / "native/mobile/NOTICE",
                   "LICENSE.sing-box": upstream, "LICENSE.wgc": wgc}
        metadata = {**self.metadata, "artifact_sha256": artifact_hashes(aar)}
        self.entry = publish_artifact(self.producer, aar, "android", self.capabilities, metadata, notices)
        update_receipt(self.producer, self.capabilities, self.selection, {"android": self.entry})
        self.output = (self.producer / self.entry["path"]).parent
        if validate:
            write_manifest(self.producer)
        else:
            self.reseal()

    def reseal(self):
        _, _, _, files = bundle(self.producer)
        write_json(self.producer / MANIFEST, {"schema": 1, "sha256": files})

    def assert_miss(self):
        before = (self.host / "pubspec.yaml").read_bytes()
        with self.assertRaises(RestoreMiss):
            restore_artifact(self.producer, self.host)
        self.assertEqual(self.receipt_path.read_bytes(), self.previous_receipt)
        self.assertEqual((self.host / "pubspec.yaml").read_bytes(), before)

    def test_complete_bundle_restores_without_running_a_compiler(self):
        with mock.patch("subprocess.run", side_effect=AssertionError("No subprocess is needed for reuse")):
            restore_artifact(self.producer, self.host)
        manifest = json.loads((self.producer / MANIFEST).read_text())
        for relative, digest in manifest["sha256"].items():
            self.assertEqual(sha256(self.host / relative), digest)
        self.assertEqual(self.receipt_path.read_bytes(), (self.producer / RECEIPT).read_bytes())
        self.assertTrue(any(name.endswith("/Legal/LICENSE.wgc") for name in manifest["sha256"]))
        self.assertTrue(any(name.endswith("/assets/sing_box_flutter/core-capabilities.json")
                            for name in manifest["sha256"]))

    def test_existing_matching_bundle_does_not_copy_the_binary_again(self):
        with mock.patch("restore_core_artifact.shutil.copytree", wraps=shutil.copytree) as copy:
            restore_artifact(self.producer, self.host)
            self.assertGreater(copy.call_count, 0)
            copy.reset_mock()
            restore_artifact(self.producer, self.host)
            self.assertEqual(copy.call_count, 0)
        self.assertEqual(self.receipt_path.read_bytes(), (self.producer / RECEIPT).read_bytes())

    def test_missing_selected_bundle_leaves_existing_host_untouched(self):
        shutil.rmtree(self.producer / ".dart_tool")
        self.assert_miss()

    def test_changed_host_pubspec_is_never_rewritten_to_accept_a_bundle(self):
        with (self.host / "pubspec.yaml").open("a") as handle:
            handle.write("# Different checkout or selected host profile\n")
        self.assert_miss()

    def test_recipe_staleness_is_rejected_even_with_valid_bundle_checksums(self):
        metadata_path = self.output / "native-build.json"
        metadata = json.loads(metadata_path.read_text())
        name = next(iter(metadata["identity"]["mobile_sources"]))
        metadata["identity"]["mobile_sources"][name] = "0" * 64
        write_json(metadata_path, metadata)
        self.reseal()
        self.assert_miss()

    def test_single_abi_bundle_cannot_replace_a_four_abi_build(self):
        self.export(("arm64-v8a",), validate=False)
        self.assert_miss()
        with self.assertRaisesRegex(BuildError, "four Android ABIs"):
            write_manifest(self.producer)

    def test_every_bundle_file_is_covered_by_the_manifest(self):
        for relative in ("libbox.aar", "core-capabilities.json", "native-build.json",
                         "assets/sing_box_flutter/core-capabilities.json", "Legal/LICENSE.wgc"):
            path = self.output / relative
            original = path.read_bytes()
            with self.subTest(relative=relative):
                path.write_bytes(b"changed")
                self.assert_miss()
            path.write_bytes(original)

    def test_resealed_aar_or_capability_tampering_still_fails_receipt_checks(self):
        for relative in ("libbox.aar", "core-capabilities.json",
                         "assets/sing_box_flutter/core-capabilities.json"):
            path = self.output / relative
            original = path.read_bytes()
            with self.subTest(relative=relative):
                path.write_bytes(b"changed")
                self.reseal()
                self.assert_miss()
            path.write_bytes(original)
        self.reseal()

    def test_resealed_bundle_still_requires_dependency_licenses(self):
        (self.output / "Legal/LICENSE.wgc").unlink()
        self.reseal()
        self.assert_miss()

    def test_changed_current_notices_are_incompatible(self):
        (self.output / "Legal/THIRD_PARTY_NOTICES.md").write_text("old notices\n")
        self.reseal()
        self.assert_miss()

    def test_selected_bundle_layout_cannot_escape_or_follow_symlinks(self):
        receipt = json.loads((self.producer / RECEIPT).read_text())
        for path in ("../libbox.aar", "/tmp/libbox.aar", ".dart_tool/other/libbox.aar"):
            with self.subTest(path=path):
                receipt["artifacts"]["android"]["path"] = path
                write_json(self.producer / RECEIPT, receipt)
                self.assert_miss()
        receipt["artifacts"]["android"]["path"] = self.entry["path"]
        write_json(self.producer / RECEIPT, receipt)
        legal = self.output / "Legal/LICENSE.wgc"
        legal.unlink()
        legal.symlink_to(self.base / "LICENSE.wgc")
        self.assert_miss()

    def test_new_unexpected_bundle_file_is_not_accepted_by_resealing(self):
        (self.output / "extra.py").write_text("unexpected\n")
        self.reseal()
        self.assert_miss()

    def test_copying_changed_bundle_does_not_publish_a_receipt(self):
        original_copytree = shutil.copytree

        def copytree(source, destination, *args, **kwargs):
            result = original_copytree(source, destination, *args, **kwargs)
            if Path(source) == self.output:
                (Path(destination) / "Legal/LICENSE.wgc").write_text("changed during copy\n")
            return result

        with mock.patch("restore_core_artifact.shutil.copytree", side_effect=copytree):
            self.assert_miss()
        self.assertFalse(list(self.host.rglob(".restore-*")))

    def test_failed_receipt_publication_preserves_previous_receipt(self):
        with mock.patch("restore_core_artifact.os.replace", side_effect=OSError("Interrupted receipt publication")):
            with self.assertRaises(OSError):
                restore_artifact(self.producer, self.host)
        self.assertEqual(self.receipt_path.read_bytes(), self.previous_receipt)
        self.assertFalse(list(self.host.rglob(".restore-*")))

    def test_cli_reports_fallback_without_failing_the_job(self):
        (self.producer / MANIFEST).unlink()
        output = self.base / "github-output"
        with mock.patch.dict(os.environ, {"GITHUB_OUTPUT": str(output)}):
            self.assertEqual(main(["--project-dir", str(self.host), "--from-dir", str(self.producer)]), 0)
        self.assertEqual(output.read_text(), "restored=false\n")
        self.assertEqual(self.receipt_path.read_bytes(), self.previous_receipt)


if __name__ == "__main__":
    unittest.main()
