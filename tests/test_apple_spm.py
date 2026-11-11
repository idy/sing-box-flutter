"""Host-private SPM staging and prebuild guards, without Go builds or downloads."""
import copy
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
import apple_spm
from build_core import RECEIPT
from native_common import BuildError, ROOT, artifact_hashes, sha256


class AppleSPM(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="apple spm ")
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.host = self.base / "host with spaces"
        self.root = self.base / "plugin with spaces"
        self.host.mkdir()
        self.root.mkdir()
        (self.host / "pubspec.yaml").write_text("name: spm_fixture\n")
        for relative in ("ios/Classes/Shared", "ios/Classes/PacketTunnel", "macos/Classes/UserTun"):
            shutil.copytree(ROOT / relative, self.root / relative)
        for relative in (apple_spm.TEMPLATE, "tool/apple_spm.py", apple_spm.PRIVACY, apple_spm.CONTROLLER):
            destination = self.root / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / relative, destination)
        for platform in ("ios", "macos"):
            facade = self.root / platform / "sing_box_flutter"
            facade.mkdir(parents=True, exist_ok=True)
            (facade / "Package.swift").write_text("// fixture facade\n")
        self.cache = self.host / ".dart_tool/sing_box_flutter/builds/fixture"
        self.cache.mkdir(parents=True)
        self.artifact = self.cache / "Libbox.xcframework"
        self.artifact.mkdir()
        (self.artifact / "Info.plist").write_text("fixture plist")
        (self.artifact / "Libbox").write_bytes(b"fixture native bytes")
        capabilities = {"schema": 1, "version": "1.0", "fingerprint": "a" * 64, "features": {}}
        self.capabilities = self.cache / "core-capabilities.json"
        self.capabilities.write_text(json.dumps(capabilities))
        (self.cache / "metadata.json").write_text('{"schema": 1}')
        relative = self.cache.relative_to(self.host).as_posix()
        entry = {"path": relative + "/Libbox.xcframework", "metadata": relative + "/metadata.json",
                 "capabilities": relative + "/core-capabilities.json",
                 "capabilities_sha256": sha256(self.capabilities), "sha256": artifact_hashes(self.artifact)}
        self.receipt = {**capabilities, "pubspec_sha256": sha256(self.host / "pubspec.yaml"),
                        "requested_features": {}, "artifacts": {"apple": entry, "macos": copy.deepcopy(entry)}}
        self.save_receipt()
        # Recipe construction is tested by test_core_recipe; keep these fixtures
        # narrow, while exercising actual receipt, artifact and source tampering.
        self.recipe = mock.patch("core_recipe.verify_current_recipe", side_effect=self.verify_recipe).start()
        self.addCleanup(mock.patch.stopall)

    def verify_recipe(self, project, receipt, metadata, *, target, root):
        self.assertEqual(root, self.root)
        if sha256(project / "pubspec.yaml") != receipt["pubspec_sha256"]:
            raise BuildError("stale pubspec")

    def save_receipt(self):
        (self.host / RECEIPT).write_text(json.dumps(self.receipt))

    def prepare(self, target="apple", mode=None):
        if mode is not None:
            self.receipt["macos"] = {"mode": mode}
            self.save_receipt()
        return apple_spm.prepare(self.host, self.receipt, target, root=self.root)

    def validate(self, package):
        apple_spm.validate(package / "selection.json", self.base / "plugin output")

    def test_ne_targets_and_repeat_stability(self):
        for target in ("apple", "macos"):
            with self.subTest(target=target):
                package = self.prepare(target)
                self.assertEqual(package.parent.name, "Packages")
                self.assertFalse(package.is_symlink())
                controller = package / "Sources/SingBoxFlutterHost"
                extension = package / "Sources/SingBoxFlutterPacketTunnel"
                self.assertTrue((controller / "ConfigPreflight.swift").is_file())
                self.assertFalse((controller / "NativeCore.swift").exists())
                self.assertTrue((extension / "NativeCore.swift").is_file())
                self.assertFalse((extension / "SingBoxFlutterPlugin.swift").exists())
                self.assertTrue((extension / "PrivacyInfo.xcprivacy").is_file())
                manifest = (package / "Package.swift").read_text()
                self.assertIn('dependencies: [.product(name: "FlutterFramework", package: "FlutterFramework")]', manifest)
                self.assertIn('name: "SingBoxFlutterPacketTunnel", dependencies: ["Libbox"]', manifest)
                before = (package / "Package.swift").stat().st_mtime_ns
                self.assertEqual(self.prepare(target), package)
                self.assertEqual((package / "Package.swift").stat().st_mtime_ns, before)
                self.validate(package)
                generated = self.base / "plugin output/GeneratedCapabilities.swift"
                before = generated.stat().st_mtime_ns
                self.validate(package)
                self.assertEqual(generated.stat().st_mtime_ns, before)
                self.assertIn("SBFSelectedCapabilities", generated.read_text())

    def test_user_tun_split(self):
        package = self.prepare("macos", "user_tun")
        controller = package / "Sources/SingBoxFlutterHost"
        self.assertTrue((controller / "NativeCore.swift").is_file())
        self.assertTrue((controller / "UserTunRuntime.swift").is_file())
        self.assertFalse((controller / "ConfigPreflight.swift").exists())
        self.assertFalse((package / "Sources/SingBoxFlutterPacketTunnel").exists())
        manifest = (package / "Package.swift").read_text()
        self.assertNotIn("NetworkExtension", manifest)
        self.assertNotIn("sing-box-flutter-packet-tunnel", manifest)
        self.assertIn('package: "FlutterFramework"), "Libbox"]', manifest)
        self.validate(package)

    def test_rejects_stale_pubspec(self):
        package = self.prepare()
        (self.host / "pubspec.yaml").write_text("name: changed\n")
        with self.assertRaises(BuildError):
            self.validate(package)

    def test_rejects_modified_artifact_and_capabilities(self):
        for path in (self.artifact / "Libbox", self.capabilities):
            with self.subTest(path=path.name):
                package = self.prepare()
                before = path.read_bytes()
                path.write_bytes(b"tampered")
                with self.assertRaises(BuildError):
                    self.validate(package)
                path.write_bytes(before)

    def test_rejects_changed_installed_selection(self):
        package = self.prepare("macos")
        self.receipt["macos"] = {"mode": "user_tun"}
        self.save_receipt()
        with self.assertRaises(BuildError):
            self.validate(package)
        replacement = self.prepare("macos")
        self.validate(replacement)
        self.assertFalse((replacement / "Sources/SingBoxFlutterPacketTunnel").exists())

    def test_rejects_staged_and_canonical_tampering(self):
        for relative, canonical in (("Sources/SingBoxFlutterHost/SharedTypes.swift", False),
                                     ("Package.swift", False), ("Plugins/Validate/Validate.swift", False),
                                     ("ios/Classes/Shared/SharedTypes.swift", True),
                                     ("ios/sing_box_flutter/Package.swift", True),
                                     (apple_spm.TEMPLATE, True), ("tool/apple_spm.py", True)):
            with self.subTest(relative=relative):
                package = self.prepare()
                path = (self.root if canonical else package) / relative
                before = path.read_bytes()
                path.write_bytes(before + b"\n// modified\n")
                with self.assertRaises(BuildError):
                    self.validate(package)
                path.write_bytes(before)

    def test_rejects_extra_staged_source_and_settings_tampering(self):
        package = self.prepare()
        extra = package / "Sources/SingBoxFlutterHost/Unexpected.swift"
        extra.write_text("// unexpected")
        with self.assertRaises(BuildError):
            self.validate(package)
        extra.unlink()
        settings = package / "selection.json"
        data = json.loads(settings.read_text())
        data["mode"] = "user_tun"
        settings.write_text(json.dumps(data))
        with self.assertRaises(BuildError):
            self.validate(package)

    def test_rejects_new_valid_artifact_pointer(self):
        package = self.prepare()
        replacement = self.cache / "replacement/Libbox.xcframework"
        shutil.copytree(self.artifact, replacement)
        self.receipt["artifacts"]["apple"]["path"] = replacement.relative_to(self.host).as_posix()
        self.save_receipt()
        with self.assertRaises(BuildError):
            self.validate(package)

    def test_isolated_cli_imports_and_fails_closed(self):
        result = subprocess.run([sys.executable, "-I", str(ROOT / "tool/apple_spm.py"),
                                 "validate", "--settings", str(self.base / "missing settings.json"),
                                 "--output", str(self.base / "unused output")], capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("Selected Apple Swift package is stale or invalid", result.stderr)
        self.assertNotIn("Traceback", result.stderr)
        self.assertFalse((self.base / "unused output").exists())

    def test_rejects_changed_binary_link(self):
        package = self.prepare()
        link = next((package / "Artifacts").glob("*/Libbox.xcframework"))
        link.unlink()
        link.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(BuildError):
            self.validate(package)

    def test_rejects_parent_and_source_symlinks(self):
        outside = self.base / "outside"
        outside.mkdir()
        (self.host / "ios").symlink_to(outside, target_is_directory=True)
        with self.assertRaises(BuildError):
            self.prepare()
        self.assertEqual(list(outside.iterdir()), [])
        (self.host / "ios").unlink()
        package = self.prepare()
        source = package / "Sources/SingBoxFlutterHost/SharedTypes.swift"
        source.unlink()
        source.symlink_to(self.root / "ios/Classes/Shared/SharedTypes.swift")
        with self.assertRaises(BuildError):
            self.prepare()
        self.assertTrue(source.is_symlink())

    def test_missing_selection_and_unpublished_receipt(self):
        del self.receipt["artifacts"]["apple"]
        self.save_receipt()
        with self.assertRaisesRegex(BuildError, "selected core"):
            self.prepare()
        self.receipt["fingerprint"] = "b" * 64
        with self.assertRaisesRegex(BuildError, "already updated"):
            self.prepare()

    def test_atomic_replacement_restores_previous_on_failure(self):
        package = self.prepare("macos")
        old = (package / "selection.json").read_bytes()
        self.receipt["macos"] = {"mode": "user_tun"}
        self.save_receipt()
        rename = Path.rename

        def fail_publish(path, destination):
            if path.name.startswith(".SingBoxFlutterHost-") and not path.name.endswith("-previous"):
                raise OSError("injected rename failure")
            return rename(path, destination)

        with mock.patch.object(Path, "rename", fail_publish), self.assertRaises(OSError):
            self.prepare("macos")
        self.assertEqual((package / "selection.json").read_bytes(), old)
        self.assertTrue((package / "Sources/SingBoxFlutterPacketTunnel").is_dir())

    @unittest.skipUnless(shutil.which("swift"), "Swift is unavailable")
    def test_manifest_parses_all_modes(self):
        for target, mode in (("apple", "ne"), ("macos", "ne"), ("macos", "user_tun")):
            with self.subTest(target=target, mode=mode):
                package = self.prepare(target, mode)
                flutter = package.parent / ".packages/FlutterFramework"
                flutter.mkdir(parents=True, exist_ok=True)
                (flutter / "Package.swift").write_text('// swift-tools-version: 5.9\nimport PackageDescription\n'
                    'let package = Package(name: "FlutterFramework", products: [], targets: [])\n')
                result = subprocess.run(["swift", "package", "--package-path", str(package), "dump-package"],
                                        capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                parsed = json.loads(result.stdout)
                self.assertEqual(parsed["name"], "SingBoxFlutterHost")
                targets = {entry["name"]: entry for entry in parsed["targets"]}
                self.assertEqual(targets["SingBoxFlutterValidate"]["type"], "plugin")


if __name__ == "__main__":
    unittest.main()
