"""Host artifact isolation, receipts and interrupted-build recovery."""
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock
import zipfile
import shutil

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
from build_core import (RECEIPT, canonical_hash, host_path, invalidate_pods, load_host_build_request,
                        mobile_overlay, mobile_sources, publish_artifact, update_receipt)
from native_common import BuildError


class HostArtifacts(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.host = self.root / "host"
        self.host.mkdir()
        (self.host / "pubspec.yaml").write_text("name: host\n")
        self.capabilities = {"schema": 1, "version": "1.14.4", "fingerprint": "a" * 64,
                             "features": {"endpoints": ["wgc"]}}
        self.aar = self.root / "libbox.aar"
        with zipfile.ZipFile(self.aar, "w") as archive:
            archive.writestr("classes.jar", "classes")
            archive.writestr("AndroidManifest.xml", "manifest")
            archive.writestr("jni/arm64-v8a/libbox.so", "native")

    def publish(self):
        return publish_artifact(self.host, self.aar, "android", self.capabilities,
                                {"target": "android"}, {})

    def test_publication_is_host_local_and_tamper_evident(self):
        entry = self.publish()
        artifact = host_path(self.host, entry["path"])
        self.assertEqual(artifact.read_bytes(), self.aar.read_bytes())
        self.assertTrue(host_path(self.host, entry["capabilities"]).is_file())
        self.assertEqual(self.publish(), entry)
        artifact.write_bytes(b"changed")
        with self.assertRaises(BuildError):
            self.publish()

    def test_capabilities_asset_is_identical_to_receipt_source(self):
        entry = self.publish()
        path = host_path(self.host, entry["path"])
        bundled = path.parent / "assets/sing_box_flutter/core-capabilities.json"
        self.assertEqual(bundled.read_bytes(), host_path(self.host, entry["capabilities"]).read_bytes())

    def test_paths_cannot_escape_or_use_symlinks(self):
        for path in ["../other", "/tmp/native", ".dart_tool/sing_box_flutter/../other", "android/libs/libbox.aar"]:
            with self.subTest(path=path), self.assertRaises(BuildError):
                host_path(self.host, path)
        (self.host / ".dart_tool").symlink_to(self.root / "elsewhere")
        with self.assertRaises(BuildError):
            host_path(self.host, RECEIPT.as_posix())

    def test_new_profile_drops_other_profiles_platforms(self):
        entry = self.publish()
        update_receipt(self.host, self.capabilities, {}, {"android": entry})
        changed = {**self.capabilities, "fingerprint": "b" * 64}
        update_receipt(self.host, changed, {}, {})
        receipt = json.loads((self.host / RECEIPT).read_text())
        self.assertEqual(receipt["artifacts"], {})

    def test_tampered_retained_platform_is_not_blessed(self):
        entry = self.publish()
        update_receipt(self.host, self.capabilities, {}, {"android": entry})
        host_path(self.host, entry["capabilities"]).write_text("{}")
        with self.assertRaises(BuildError):
            update_receipt(self.host, self.capabilities, {}, {})

    def test_profile_switch_during_build_preserves_previous_receipt(self):
        from native_common import sha256
        entry = self.publish()
        update_receipt(self.host, self.capabilities, {}, {"android": entry})
        previous = (self.host / RECEIPT).read_bytes()
        expected = sha256(self.host / "pubspec.yaml")
        (self.host / "pubspec.yaml").write_text("name: changed_profile\n")
        with self.assertRaisesRegex(BuildError, "changed during"):
            update_receipt(self.host, self.capabilities, {}, {"android": entry}, expected)
        self.assertEqual((self.host / RECEIPT).read_bytes(), previous)

    def test_pod_refresh_keeps_the_checked_in_dependency_lock(self):
        pods = self.host / "ios/Pods"
        pods.mkdir(parents=True)
        sandbox = pods / "Manifest.lock"
        sandbox.write_text("PODS: selected core\n")
        locked = pods.parent / "Podfile.lock"
        locked.write_text("PODS: locked versions\n")
        invalidate_pods(self.host)
        self.assertFalse(sandbox.exists())
        self.assertEqual(locked.read_text(), "PODS: locked versions\n")
        invalidate_pods(self.host)  # No installed pods yet is harmless.

    def test_pod_refresh_never_deletes_a_shared_external_sandbox(self):
        external = self.root / "shared-pods"
        external.mkdir()
        sandbox = external / "Manifest.lock"
        sandbox.write_text("shared\n")
        (self.host / "ios").mkdir()
        (self.host / "ios/Pods").symlink_to(external)
        with self.assertRaises(BuildError):
            invalidate_pods(self.host)
        self.assertEqual(sandbox.read_text(), "shared\n")

    def publish_apple(self, target):
        output = self.root / f"{target}-Libbox.xcframework"
        output.mkdir(exist_ok=True)
        info = output / "Info.plist"
        if not info.exists():
            info.write_text("fixture framework metadata")
        return publish_artifact(self.host, output, target, self.capabilities, {"target": target}, {})

    def install_pods(self, target, platform, *, selected=True, receipt=None):
        receipt = receipt or json.loads((self.host / RECEIPT).read_text())
        shared = {"name": "Shared"}
        if selected:
            marker = {"schema": 1, "target": target, "fingerprint": receipt["fingerprint"],
                      "pubspec_sha256": receipt["pubspec_sha256"], "macos": receipt.get("macos"),
                      "artifact": receipt["artifacts"][target]}
            shared["script_phases"] = [{"name": "Validate selected core artifacts",
                                        "execution_position": "before_compile", "always_out_of_date": "1",
                                        "script": "set -eu\n# sing_box_flutter pod selection: " + json.dumps(marker) + "\n"}]
        pods = self.host / platform / "Pods"
        local_specs = pods / "Local Podspecs"
        local_specs.mkdir(parents=True, exist_ok=True)
        data = json.dumps({"name": "sing_box_flutter", "subspecs": [shared]}).encode()
        (local_specs / "sing_box_flutter.podspec.json").write_bytes(data)
        locked = ("PODS:\n  - sing_box_flutter (0.1.0)\n\nSPEC CHECKSUMS:\n"
                  f"  sing_box_flutter: {hashlib.sha1(data).hexdigest()}\n\nCOCOAPODS: 1.17.0\n")
        (pods.parent / "Podfile.lock").write_text(locked)
        manifest = pods / "Manifest.lock"
        manifest.write_text(locked)
        return manifest

    def test_identical_apple_receipts_keep_installed_pods(self):
        for target, platform in (("apple", "ios"), ("macos", "macos")):
            with self.subTest(target=target):
                entry = self.publish_apple(target)
                update_receipt(self.host, self.capabilities, {}, {target: entry})
                manifest = self.install_pods(target, platform)
                locked = manifest.read_bytes()
                update_receipt(self.host, self.capabilities, {}, {target: entry})
                self.assertEqual(manifest.read_bytes(), locked)
                self.assertEqual((self.host / platform / "Podfile.lock").read_bytes(), locked)

    def test_restored_selection_refreshes_legacy_pods_even_with_identical_receipt(self):
        entry = self.publish_apple("apple")
        selected_pubspec = "name: host\nsing_box_flutter:\n  core: {}\n"
        for rebuilt_target in ("apple", "android"):
            with self.subTest(rebuilt_target=rebuilt_target):
                (self.host / "pubspec.yaml").write_text(selected_pubspec)
                update_receipt(self.host, self.capabilities, {}, {"apple": entry})
                self.install_pods("apple", "ios")
                previous = (self.host / RECEIPT).read_bytes()
                # Legacy pod install ignores the obsolete selected receipt.
                (self.host / "pubspec.yaml").write_text("name: host\n")
                manifest = self.install_pods("apple", "ios", selected=False)
                locked = manifest.read_bytes()
                (self.host / "pubspec.yaml").write_text(selected_pubspec)
                artifacts = {"apple": self.publish_apple("apple")} if rebuilt_target == "apple" else {"android": self.publish()}
                update_receipt(self.host, self.capabilities, {}, artifacts)
                self.assertFalse(manifest.exists())
                self.assertEqual((self.host / "ios/Podfile.lock").read_bytes(), locked)
                if rebuilt_target == "apple":
                    self.assertEqual((self.host / RECEIPT).read_bytes(), previous)
                # After pod install restores the selected guard, another identical
                # build must leave the installation alone, including retained iOS.
                self.install_pods("apple", "ios")
                installed = manifest.read_bytes()
                update_receipt(self.host, self.capabilities, {}, artifacts)
                self.assertEqual(manifest.read_bytes(), installed)

    def test_installed_guard_must_match_the_receipt_and_completed_installation(self):
        entry = self.publish_apple("macos")
        update_receipt(self.host, self.capabilities, {}, {"macos": entry})
        for invalid in ("stale selection", "uncommitted podspec", "mismatched locks", "missing guard"):
            with self.subTest(invalid=invalid):
                receipt = json.loads((self.host / RECEIPT).read_text())
                if invalid == "stale selection":
                    receipt["macos"] = {"mode": "user_tun"}
                manifest = self.install_pods("macos", "macos", receipt=receipt)
                if invalid == "uncommitted podspec":
                    # An interrupted pod install can replace Local Podspecs before
                    # it updates the Xcode project and publishes the new locks.
                    spec = manifest.parent / "Local Podspecs/sing_box_flutter.podspec.json"
                    spec.write_bytes(spec.read_bytes() + b"\n")
                elif invalid == "mismatched locks":
                    manifest.write_text(manifest.read_text() + "# incomplete install\n")
                elif invalid == "missing guard":
                    self.install_pods("macos", "macos", selected=False)
                locked = (self.host / "macos/Podfile.lock").read_bytes()
                update_receipt(self.host, self.capabilities, {}, {"macos": entry})
                self.assertFalse(manifest.exists())
                self.assertEqual((self.host / "macos/Podfile.lock").read_bytes(), locked)

    def test_identical_receipt_refresh_failure_retries_all_stale_apple_pods(self):
        entries = {target: self.publish_apple(target) for target in ("apple", "macos")}
        update_receipt(self.host, self.capabilities, {}, entries)
        before = (self.host / RECEIPT).read_bytes()
        ios_manifest = self.install_pods("apple", "ios", selected=False)
        outside = self.root / "shared-pods"
        outside.mkdir()
        (outside / "Manifest.lock").write_text("external legacy installation")
        macos = self.host / "macos"
        macos.mkdir()
        (macos / "Pods").symlink_to(outside)
        with self.assertRaises(BuildError):
            update_receipt(self.host, self.capabilities, {}, entries)
        self.assertFalse(ios_manifest.exists())
        self.assertEqual((self.host / RECEIPT).read_bytes(), before)
        self.assertEqual((outside / "Manifest.lock").read_text(), "external legacy installation")
        (macos / "Pods").unlink()
        macos_manifest = self.install_pods("macos", "macos", selected=False)
        # A retry must check installed state again, even though the receipt is equal.
        update_receipt(self.host, self.capabilities, {}, entries)
        self.assertFalse(ios_manifest.exists())
        self.assertFalse(macos_manifest.exists())
        self.assertEqual((self.host / RECEIPT).read_bytes(), before)

    def test_changed_native_artifact_invalidates_only_its_platform(self):
        entry = self.publish_apple("macos")
        update_receipt(self.host, self.capabilities, {}, {"macos": entry})
        for platform in ("macos", "ios"):
            pods = self.host / platform / "Pods"
            pods.mkdir(parents=True)
            (pods / "Manifest.lock").write_text("installed selection")
        (self.root / "macos-Libbox.xcframework/Info.plist").write_text("rebuilt framework")
        rebuilt = self.publish_apple("macos")
        self.assertNotEqual(rebuilt["path"], entry["path"])
        update_receipt(self.host, self.capabilities, {}, {"macos": rebuilt})
        self.assertFalse((self.host / "macos/Pods/Manifest.lock").exists())
        self.assertEqual((self.host / "ios/Pods/Manifest.lock").read_text(), "installed selection")

    def test_metadata_change_refreshes_both_retained_apple_artifacts(self):
        entries = {target: self.publish_apple(target) for target in ("apple", "macos")}
        update_receipt(self.host, self.capabilities, {}, entries)
        for platform in ("ios", "macos"):
            pods = self.host / platform / "Pods"
            pods.mkdir(parents=True)
            (pods / "Manifest.lock").write_text("old pubspec selection")
            (pods.parent / "Podfile.lock").write_text("locked versions")
        (self.host / "pubspec.yaml").write_text("name: changed_metadata\n")
        update_receipt(self.host, self.capabilities, {}, {"android": self.publish()})
        receipt = json.loads((self.host / RECEIPT).read_text())
        self.assertEqual(set(receipt["artifacts"]), {"android", "apple", "macos"})
        for platform in ("ios", "macos"):
            self.assertFalse((self.host / platform / "Pods/Manifest.lock").exists())
            self.assertEqual((self.host / platform / "Podfile.lock").read_text(), "locked versions")

    def test_profile_change_refreshes_pods_for_removed_apple_selections(self):
        entries = {target: self.publish_apple(target) for target in ("apple", "macos")}
        update_receipt(self.host, self.capabilities, {}, entries)
        for platform in ("ios", "macos"):
            pods = self.host / platform / "Pods"
            pods.mkdir(parents=True)
            (pods / "Manifest.lock").write_text("retired selection")
        changed = {**self.capabilities, "fingerprint": "b" * 64}
        entry = publish_artifact(self.host, self.aar, "android", changed, {}, {})
        update_receipt(self.host, changed, {}, {"android": entry})
        self.assertEqual(set(json.loads((self.host / RECEIPT).read_text())["artifacts"]), {"android"})
        for platform in ("ios", "macos"):
            self.assertFalse((self.host / platform / "Pods/Manifest.lock").exists())

    def test_pubspec_change_refreshes_pods_even_for_cached_native_bytes(self):
        entry = self.publish_apple("macos")
        update_receipt(self.host, self.capabilities, {}, {"macos": entry})
        pods = self.host / "macos/Pods"
        pods.mkdir(parents=True)
        (pods / "Manifest.lock").write_text("installed selection")
        (self.host / "pubspec.yaml").write_text("name: changed_host_metadata\n")
        update_receipt(self.host, self.capabilities, {}, {"macos": entry})
        self.assertFalse((pods / "Manifest.lock").exists())

    def test_failed_pods_refresh_preserves_receipt_for_a_later_retry(self):
        entry = self.publish_apple("macos")
        update_receipt(self.host, self.capabilities, {}, {"macos": entry})
        before = (self.host / RECEIPT).read_bytes()
        outside = self.root / "outside-pods"
        outside.mkdir()
        (outside / "Manifest.lock").write_text("external installation")
        native = self.host / "macos"
        native.mkdir()
        pods = native / "Pods"
        pods.symlink_to(outside)
        (self.host / "pubspec.yaml").write_text("name: changed_host_metadata\n")
        with self.assertRaises(BuildError):
            update_receipt(self.host, self.capabilities, {}, {"macos": entry})
        self.assertEqual((self.host / RECEIPT).read_bytes(), before)
        self.assertEqual((outside / "Manifest.lock").read_text(), "external installation")
        pods.unlink()
        pods.mkdir()
        (pods / "Manifest.lock").write_text("stale local installation")
        update_receipt(self.host, self.capabilities, {}, {"macos": entry})
        self.assertFalse((pods / "Manifest.lock").exists())
        self.assertNotEqual((self.host / RECEIPT).read_bytes(), before)

    def request(self, features=None):
        from native_common import sha256
        document = {"schema": 1, "pubspec": {"path": str((self.host / "pubspec.yaml").resolve()),
                     "sha256": sha256(self.host / "pubspec.yaml")}, "features": features or {}}
        path = self.root / "request.json"
        path.write_text(json.dumps(document))
        return path, document

    def test_request_uses_the_exact_captured_pubspec(self):
        path, document = self.request({"endpoints": ["wgc"]})
        features, digest, macos = load_host_build_request(path, self.host)
        self.assertIsNone(macos)
        self.assertEqual(features["endpoints"], ["wgc"])
        self.assertEqual(features["dns"], ["local", "tcp", "udp"])
        self.assertEqual(digest, document["pubspec"]["sha256"])
        (self.host / "pubspec.yaml").write_text("name: changed_before_python\n")
        with self.assertRaisesRegex(BuildError, "snapshot"):
            load_host_build_request(path, self.host)

    def test_request_utf8_non_ascii_path_matches_captured_host_bytes(self):
        from native_common import sha256
        host = self.root / "应用 项目"
        host.mkdir()
        pubspec = host / "pubspec.yaml"
        pubspec.write_text("name: host\n# 中文配置\n", encoding="utf-8")
        document = {"schema": 1, "pubspec": {"path": str(pubspec.resolve()), "sha256": sha256(pubspec)},
                    "features": {"endpoints": ["wgc"]}}
        request = host / "请求.json"
        request.write_text(json.dumps(document, ensure_ascii=False), encoding="utf-8")
        self.assertIn("应用 项目".encode("utf-8"), request.read_bytes())
        features, captured, macos = load_host_build_request(request, host)
        self.assertEqual(features["endpoints"], ["wgc"])
        self.assertEqual(captured, sha256(pubspec))
        self.assertIsNone(macos)

    def test_request_rejects_unbound_or_alternate_manifest(self):
        from native_common import sha256
        path, document = self.request()
        path.write_text(json.dumps({"endpoints": ["wgc"]}))
        with self.assertRaises(BuildError):
            load_host_build_request(path, self.host)
        alternate = self.host / "selection.yaml"
        alternate.write_text("name: alternate\n")
        document["pubspec"] = {"path": str(alternate), "sha256": sha256(alternate)}
        path.write_text(json.dumps(document))
        with self.assertRaises(BuildError):
            load_host_build_request(path, self.host)

    def test_request_rejects_duplicate_keys(self):
        path, _ = self.request()
        path.write_text('{"schema":1,"schema":1,"pubspec":{},"features":{}}')
        with self.assertRaises(BuildError):
            load_host_build_request(path, self.host)

    def test_cache_hit_skips_duplicate_binary_copy(self):
        entry = self.publish()
        with mock.patch("build_core.shutil.copyfile", wraps=shutil.copyfile) as copying:
            self.assertEqual(self.publish(), entry)
        self.assertFalse(any(Path(call.args[0]) == self.aar for call in copying.call_args_list))
        with self.assertRaises(TypeError):
            publish_artifact(self.host, self.aar, "android", self.capabilities, {"bad": object()}, {})
        with self.assertRaises(FileNotFoundError):
            publish_artifact(self.host, self.aar, "android", self.capabilities, {}, {"missing": self.root / "absent"})

    def test_cache_hit_rejects_missing_or_modified_android_asset(self):
        entry = self.publish()
        directory = host_path(self.host, entry["path"]).parent
        asset = directory / "assets/sing_box_flutter/core-capabilities.json"
        original = asset.read_bytes()
        asset.unlink()
        with self.assertRaisesRegex(BuildError, "capabilities asset"):
            self.publish()
        asset.write_text("{\"changed\":true}\n")
        with self.assertRaisesRegex(BuildError, "capabilities asset"):
            self.publish()
        asset.write_bytes(original)
        self.assertEqual(self.publish(), entry)

    def test_cache_hit_repairs_corrupt_provenance_and_stale_notices(self):
        notice = self.root / "NOTICE"
        notice.write_text("first notice\n")
        metadata = {"schema": 1, "target": "android", "sdks": {"ndk": "first"}}
        publish = lambda value: publish_artifact(self.host, self.aar, "android", self.capabilities,
                                                 value, {"NOTICE": notice})
        entry = publish(metadata)
        directory = host_path(self.host, entry["path"]).parent
        provenance = directory / "native-build.json"
        recorded = json.loads(provenance.read_text())
        # Provenance that still describes the identical binary is kept as first recorded.
        publish({**metadata, "sdks": {"ndk": "second"}})
        self.assertEqual(json.loads(provenance.read_text()), recorded)
        provenance.write_text("{corrupt")
        notice.write_text("updated notice\n")
        (directory / "Legal/STALE").write_text("stale\n")
        self.assertEqual(publish(metadata), entry)
        self.assertEqual(json.loads(provenance.read_text()), recorded)
        self.assertEqual(sorted(path.name for path in (directory / "Legal").iterdir()), ["NOTICE"])
        self.assertEqual((directory / "Legal/NOTICE").read_text(), "updated notice\n")
        self.assertEqual([path.name for path in directory.parent.iterdir() if path.name.startswith(".")], [])

    @unittest.skipIf(hasattr(os, "geteuid") and os.geteuid() == 0, "root ignores directory permissions")
    def test_read_only_package_builds_in_host_work_directory(self):
        import build_core
        package = self.root / "package"
        package.mkdir()
        with mock.patch.object(build_core, "ROOT", package):
            self.assertEqual(build_core.work_root(self.host, None), (package / ".build").resolve())
            (package / ".build").rmdir()
            package.chmod(0o555)
            self.addCleanup(package.chmod, 0o755)
            work = build_core.work_root(self.host, None)
            self.assertEqual(work, (self.host / ".dart_tool/sing_box_flutter/work").resolve())
            self.assertTrue(work.is_dir())
            self.assertFalse((package / ".build").exists())

    def test_explicit_work_directory_is_dedicated(self):
        import build_core
        with self.assertRaises(BuildError):
            build_core.work_root(self.host, "relative/work")
        busy = self.root / "busy"
        busy.mkdir()
        (busy / "user-file").write_text("keep\n")
        with self.assertRaises(BuildError):
            build_core.work_root(self.host, str(busy))
        with self.assertRaises(BuildError):
            build_core.work_root(self.host, str(build_core.ROOT / "tool"))
        work = build_core.work_root(self.host, str(self.root / "work"))
        self.assertTrue((work / build_core.WORK_MARKER).is_file())
        (work / "cache").mkdir()
        self.assertEqual(build_core.work_root(self.host, str(work)), work)
        self.assertEqual(build_core.work_directory(work / "cache", work), work / "cache")
        with self.assertRaises(BuildError):
            build_core.work_directory(self.root / "elsewhere", work)

    def test_failed_native_artifact_does_not_publish_receipt(self):
        self.aar.write_bytes(b"not a zip")
        with self.assertRaises(BuildError):
            self.publish()
        self.assertFalse((self.host / RECEIPT).exists())

    def test_source_overlay_restores_after_failure(self):
        source = self.root / "sing-box"
        original = source / "experimental/libbox"
        original.mkdir(parents=True)
        (original / "original.go").write_text("package libbox\n")
        mobile = self.root / "native/mobile"
        mobile.mkdir(parents=True)
        (mobile / "runtime.go").write_text("package libbox\n// own runtime\n")
        with self.assertRaisesRegex(RuntimeError, "interrupted"):
            with mobile_overlay(source, "package libbox\n// selected registry\n", mobile_sources(self.root), self.root):
                self.assertFalse((original / "original.go").exists())
                self.assertTrue((original / "registry_selected.go").exists())
                raise RuntimeError("interrupted")
        self.assertEqual([p.name for p in original.iterdir()], ["original.go"])

    def test_fingerprint_is_order_independent(self):
        self.assertEqual(canonical_hash({"a": 1, "b": 2}), canonical_hash({"b": 2, "a": 1}))


if __name__ == "__main__":
    unittest.main()
