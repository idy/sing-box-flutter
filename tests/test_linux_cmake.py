"""Exercise Linux receipt validation and Flutter-style nested bundle installation."""
import copy
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
from build_core import build_identity, canonical_hash, host_path, publish_artifact, update_receipt
from core_features import canonicalize, resolve_features
from core_recipe import verify_current_artifact, verify_current_recipe
from native_common import BuildError, ROOT, load_pins
from native_fixtures import linux_executable


class LinuxPackaging(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="linux package ")
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.host = self.root / "host"
        self.host.mkdir()
        (self.host / "pubspec.yaml").write_text("name: linux_fixture\nsing_box_flutter:\n  core: {}\n")
        self.selection = canonicalize({})
        self.resolved = resolve_features(self.selection, "linux")
        pins = load_pins()
        identity = build_identity(self.selection, [], pins)
        fingerprint = canonical_hash(identity)
        self.capabilities = {"schema": 1, "version": pins["sing_box_version"],
                             "fingerprint": fingerprint, "features": self.resolved["features"]}
        self.metadata = {"schema": 1, "mode": "selectable-core", "target": "linux", "identity": identity,
                         "fingerprint": fingerprint, "features": self.resolved["features"],
                         "tags": self.resolved["tags"]}
        self.entries = {}
        for arch in ("amd64", "arm64"):
            output = linux_executable(self.root / arch / "sing-box-core", arch)
            metadata = {**self.metadata, "arch": arch,
                        "environment": {"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"}}
            self.entries[f"linux-{arch}"] = publish_artifact(
                self.host, output, "linux", self.capabilities, metadata, {}, arch=arch)
        update_receipt(self.host, self.capabilities, self.selection, self.entries)
        self.receipt = json.loads((self.host / ".dart_tool/sing_box_flutter/native.json").read_text())

    def test_recipe_and_binary_must_agree_on_architecture(self):
        entry = self.entries["linux-amd64"]
        metadata = json.loads(host_path(self.host, entry["metadata"]).read_text())
        verify_current_recipe(self.host, self.receipt, metadata, target="linux", arch="amd64")
        with self.assertRaises(BuildError):
            verify_current_recipe(self.host, self.receipt, metadata, target="linux", arch="arm64")
        with self.assertRaises(BuildError):
            verify_current_recipe(self.host, self.receipt, metadata, target="linux")
        invalid = {**metadata, "environment": {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "1"}}
        with self.assertRaises(BuildError):
            verify_current_recipe(self.host, self.receipt, invalid, target="linux", arch="amd64")
        verify_current_artifact(self.host, self.receipt, "linux", arch="amd64")
        swapped = copy.deepcopy(self.receipt)
        swapped["artifacts"]["linux-amd64"] = self.entries["linux-arm64"]
        with self.assertRaises(BuildError):
            verify_current_artifact(self.host, swapped, "linux", arch="amd64")
        artifact = host_path(self.host, entry["path"])
        artifact.chmod(0o644)
        with self.assertRaises(BuildError):
            verify_current_artifact(self.host, self.receipt, "linux", arch="amd64")

    def test_validator_prints_only_verified_paths_and_digest(self):
        command = [sys.executable, "-I", str(ROOT / "tool/core_recipe.py"),
                   "--project-dir", str(self.host), "--target", "linux", "--arch", "arm64"]
        result = subprocess.run(command + ["--print-artifact-info"], text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        info = json.loads(result.stdout)
        entry = self.entries["linux-arm64"]
        self.assertEqual(info, {"path": entry["path"], "capabilities": entry["capabilities"],
                                "digest": canonical_hash(entry["sha256"])})
        guarded = command + ["--installed-artifact", entry["path"], "--installed-sha256", info["digest"]]
        self.assertEqual(subprocess.run(guarded, capture_output=True).returncode, 0)
        result = subprocess.run(guarded[:-1] + ["0" * 64], text=True, capture_output=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("CMake configuration", result.stderr)
        capabilities = host_path(self.host, entry["capabilities"])
        capabilities.write_text("{}")
        result = subprocess.run(command + ["--print-artifact-info"], text=True, capture_output=True)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, "")

    def _check_first_configure_recovery(self, *, missing_python, explicit_prefix):
        linux = self.host / "linux"
        linux.mkdir(exist_ok=True)
        failure = "python" if missing_python else "core"
        build = self.root / f"retry-{failure}-{'custom' if explicit_prefix else 'default'}"
        bundle = self.root / f"custom bundle {failure}" if explicit_prefix else build / "bundle"
        (linux / "CMakeLists.txt").write_text(
            'cmake_minimum_required(VERSION 3.19)\n'
            'project(linux_fixture LANGUAGES NONE)\n'
            'add_custom_target(flutter_assemble)\n'
            f'add_subdirectory("{ROOT / "linux"}" plugin)\n'
            'if(CMAKE_INSTALL_PREFIX_INITIALIZED_TO_DEFAULT)\n'
            f'  set(CMAKE_INSTALL_PREFIX "{build / "bundle"}" CACHE PATH "Flutter bundle" FORCE)\n'
            'endif()\n')
        command = ["cmake", "-S", str(linux), "-B", str(build), "-DFLUTTER_TARGET_PLATFORM=linux-x64"]
        first_command = command + ([f"-DCMAKE_INSTALL_PREFIX={bundle}"] if explicit_prefix else [])
        receipt = self.host / ".dart_tool/sing_box_flutter/native.json"
        contents = receipt.read_bytes()
        if not missing_python:
            receipt.unlink()
        python = self.root / "Python not installed" if missing_python else sys.executable
        try:
            failed = subprocess.run(first_command + [f"-DPython3_EXECUTABLE={python}"], capture_output=True, text=True)
            self.assertNotEqual(failed.returncode, 0, failed.stdout + failed.stderr)
            self.assertIn("Could NOT find Python3" if missing_python else "requires a selected Linux source build",
                          failed.stdout + failed.stderr)
            self.assertIn(f"CMAKE_INSTALL_PREFIX:PATH={bundle}", (build / "CMakeCache.txt").read_text())
        finally:
            if not missing_python:
                receipt.write_bytes(contents)
        configured = subprocess.run(command + [f"-DPython3_EXECUTABLE={sys.executable}"],
                                    capture_output=True, text=True)
        self.assertEqual(configured.returncode, 0, configured.stdout + configured.stderr)
        self.assertIn(f"CMAKE_INSTALL_PREFIX:PATH={bundle}", (build / "CMakeCache.txt").read_text())
        installed = subprocess.run(["cmake", "--install", str(build), "--component", "Runtime"],
                                   capture_output=True, text=True)
        self.assertEqual(installed.returncode, 0, installed.stdout + installed.stderr)
        helper = bundle / "lib/sing_box_flutter/sing-box-core"
        self.assertEqual(helper.read_bytes(), host_path(self.host, self.entries["linux-amd64"]["path"]).read_bytes())
        self.assertEqual(helper.stat().st_mode & 0o777, 0o755)
        self.assertTrue((bundle / "lib/sing_box_flutter/core-capabilities.json").is_file())

    @unittest.skipUnless(shutil.which("cmake") and sys.version_info >= (3, 11), "CMake and Python 3.11+ required")
    def test_missing_core_first_configure_does_not_cache_system_install_prefix(self):
        for explicit_prefix in (False, True):
            with self.subTest(explicit_prefix=explicit_prefix):
                self._check_first_configure_recovery(missing_python=False, explicit_prefix=explicit_prefix)

    @unittest.skipUnless(shutil.which("cmake") and sys.version_info >= (3, 11), "CMake and Python 3.11+ required")
    def test_missing_python_first_configure_does_not_cache_system_install_prefix(self):
        for explicit_prefix in (False, True):
            with self.subTest(explicit_prefix=explicit_prefix):
                self._check_first_configure_recovery(missing_python=True, explicit_prefix=explicit_prefix)

    @unittest.skipUnless(shutil.which("cmake") and sys.version_info >= (3, 11), "CMake and Python 3.11+ required")
    def test_cmake_nested_bundle_survives_flutter_cleanup_and_rechecks_artifacts(self):
        linux = self.host / "linux"
        linux.mkdir()
        # Flutter's parent includes the plugin before registering bundle cleanup.
        # Test both install order policies as well as both Flutter architectures.
        for arch, flutter_arch, policy in (("amd64", "x64", "NEW"), ("arm64", "arm64", "OLD")):
            with self.subTest(arch=arch, policy=policy):
                build = self.root / f"cmake-{arch}"
                bundle = self.root / f"bundle-{arch}"
                (linux / "CMakeLists.txt").write_text(
                    'cmake_minimum_required(VERSION 3.19)\n'
                    'project(linux_fixture LANGUAGES NONE)\n'
                    f'cmake_policy(SET CMP0082 {policy})\n'
                    'add_custom_target(flutter_assemble)\n'
                    f'add_subdirectory("{ROOT / "linux"}" plugin)\n'
                    f'install(CODE "file(REMOVE_RECURSE \\"{bundle}\\")" COMPONENT Runtime)\n'
                    f'install(FILES "{self.host / "pubspec.yaml"}" DESTINATION data COMPONENT Runtime)\n')
                configured = subprocess.run(["cmake", "-S", str(linux), "-B", str(build),
                    f"-DFLUTTER_TARGET_PLATFORM=linux-{flutter_arch}", f"-DPython3_EXECUTABLE={sys.executable}"],
                    capture_output=True, text=True)
                self.assertEqual(configured.returncode, 0, configured.stdout + configured.stderr)
                built = subprocess.run(["cmake", "--build", str(build)], capture_output=True, text=True)
                self.assertEqual(built.returncode, 0, built.stdout + built.stderr)
                install = ["cmake", "--install", str(build), "--prefix", str(bundle), "--component", "Runtime"]
                installed = subprocess.run(install, capture_output=True, text=True)
                self.assertEqual(installed.returncode, 0, installed.stdout + installed.stderr)
                helper = bundle / "lib/sing_box_flutter/sing-box-core"
                entry = self.entries[f"linux-{arch}"]
                self.assertEqual(helper.read_bytes(), host_path(self.host, entry["path"]).read_bytes())
                self.assertEqual(helper.stat().st_mode & 0o777, 0o755)
                self.assertEqual((bundle / "lib/sing_box_flutter/core-capabilities.json").read_bytes(),
                                 host_path(self.host, entry["capabilities"]).read_bytes())
                self.assertTrue((bundle / "data/pubspec.yaml").is_file())
                artifact = host_path(self.host, entry["path"])
                artifact.chmod(0o644)
                try:
                    self.assertNotEqual(subprocess.run(["cmake", "--build", str(build)], capture_output=True).returncode, 0)
                    self.assertNotEqual(subprocess.run(install, capture_output=True).returncode, 0)
                finally:
                    artifact.chmod(0o755)


if __name__ == "__main__":
    unittest.main()
