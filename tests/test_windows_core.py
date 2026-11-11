"""Windows helper build, Wintun integrity and CMake discovery without Go/network."""
import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path, PureWindowsPath
import shutil
import struct
import subprocess
import sys
import tempfile
import types
import unittest
from unittest import mock

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
import build_core
import core_recipe
from build_libbox import host_key
from core_features import FeatureError, canonicalize, generate_registry, resolve_features
from native_common import BuildError, ROOT, artifact_hashes, file_lock, load_pins, sha256, write_json
from windows_core import (WINDOWS_ARCHES, WINDOWS_EXECUTABLE, WINDOWS_SHARED_SOURCES, WINTUN_MODULE,
                          build_windows_bundle, load_wintun_lock, prepare_windows_dependencies,
                          validate_windows_bundle, verify_embedded_wintun, windows_command,
                          windows_overlay, windows_sources)
from windows_ci import needs_windows_build
from wgc_overlay import wgc_overlay


def pe(arch):
    header = bytearray(64)
    header[:2] = b"MZ"
    struct.pack_into("<I", header, 60, 64)
    return bytes(header) + b"PE\0\0" + struct.pack("<H", WINDOWS_ARCHES[arch]) + b"fixture PE\n"


def digest(content):
    return hashlib.sha256(content).hexdigest()


class WindowsSelection(unittest.TestCase):
    def test_registry_is_native_and_retains_selected_constructors(self):
        features = resolve_features({"endpoints": ["wgc"], "tls": ["reality"]}, "windows")
        registry = generate_registry(features["features"], [{"package": "example.org/transport"}], target="windows")
        for text in ("package main", "func baseContext(parent context.Context) context.Context",
                     "ctx := service.ContextWithRegistry(parent, service.NewRegistry())",
                     "return box.Context(", "wgc.RegisterEndpoint", "dns_local.RegisterTransport",
                     '_ "example.org/transport"'):
            self.assertIn(text, registry)
        for text in ("PlatformInterface", "newPlatformTransport", "filemanager", "sWorkingPath", "libbox"):
            self.assertNotIn(text, registry)
        encoded = registry.split('const selectedFeaturesJSON = `', 1)[1].split('`', 1)[0]
        self.assertEqual(json.loads(encoded), features["features"])
        self.assertNotIn("with_low_memory", features["tags"])
        self.assertIn("with_gvisor", features["tags"])
        self.assertIn("with_utls", features["tags"])

    def test_windows_rejects_linux_mobile_bridges_and_cgo_requirements(self):
        for category, name in (("inbounds", "redirect"), ("inbounds", "tproxy"),
                               ("dns", "resolved"), ("outbounds", "naive"),
                               ("outbounds", "bridge"), ("outbounds", "tor"),
                               ("services", "usbip-client")):
            with self.subTest(name=name), self.assertRaises(FeatureError):
                resolve_features({category: [name]}, "windows")

    def test_cli_architecture_and_mobile_all_are_explicit(self):
        args = build_core.parser().parse_args(["--features-file", "request.json", "--project-dir", "."])
        self.assertEqual(args.target, "all")
        self.assertEqual(args.windows_arch, "amd64")
        args = build_core.parser().parse_args(["--features-file", "request.json", "--project-dir", ".",
                                               "--target", "windows", "--windows-arch", "arm64"])
        self.assertEqual((args.target, args.windows_arch), ("windows", "arm64"))
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            build_core.parser().parse_args(["--features-file", "r", "--project-dir", ".", "--windows-arch", "386"])

    def test_native_windows_host_names(self):
        for machine, arch in (("AMD64", "amd64"), ("ARM64", "arm64"), ("aarch64", "arm64")):
            with self.subTest(machine=machine), mock.patch("build_libbox.platform.system", return_value="Windows"), mock.patch("build_libbox.platform.machine", return_value=machine):
                self.assertEqual(host_key(), f"windows-{arch}")
        lock = json.loads((ROOT / "tool/tailscale-go.json").read_text())
        self.assertEqual(lock["asset_ids"]["windows-amd64"], 470759045)
        self.assertEqual(lock["asset_ids"]["windows-arm64"], 470759138)

    def test_windows_linker_targets_standalone_main(self):
        command = windows_command(Path("go.exe"), Path(WINDOWS_EXECUTABLE), "arm64", ["with_gvisor"], load_pins())
        self.assertEqual(command[1], "build")
        self.assertEqual(command[-1], "./cmd/flutter_windows")
        linker = command[command.index("-ldflags") + 1]
        self.assertIn("-X github.com/sagernet/sing-box/constant.Version=", linker)
        self.assertNotIn("coreFingerprint", linker)
        self.assertNotIn("gomobile", str(command))
        self.assertNotIn("experimental/libbox", str(command))


class WindowsFixture(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.host = self.root / "应用 host with spaces"
        self.host.mkdir()
        (self.host / "pubspec.yaml").write_text("name: windows_host\n")
        self.plugin = self.root / "plugin"
        (self.plugin / "tool").mkdir(parents=True)
        shutil.copyfile(ROOT / "native-core.json", self.plugin / "native-core.json")
        for source in (ROOT / "tool").glob("*.py"):
            shutil.copyfile(source, self.plugin / "tool" / source.name)
        shutil.copyfile(ROOT / "tool/tailscale-go.json", self.plugin / "tool/tailscale-go.json")
        shutil.copytree(ROOT / "native/wgc", self.plugin / "native/wgc")
        helper = self.plugin / "native/windows"
        helper.mkdir()
        (helper / "main.go").write_text("package main\n// helper fixture\n", encoding="utf-8")
        mobile = self.plugin / "native/mobile"
        mobile.mkdir()
        for name in WINDOWS_SHARED_SOURCES:
            (mobile / name).write_text("// canonical shared fixture\npackage libbox\n", encoding="utf-8")
        self.lock = load_wintun_lock()
        self.contents = {"amd64": pe("amd64"), "arm64": pe("arm64"), "license": b"official license fixture\n"}
        for name, content in self.contents.items():
            self.lock["files"][name]["sha256"] = digest(content)
        (helper / "LICENSE.wintun").write_bytes(self.contents["license"])
        self.module_dir = self.root / "private module cache" / "sing-tun"
        for arch in WINDOWS_ARCHES:
            embedded = self.module_dir / f"internal/wintun/{arch}/wintun.dll"
            embedded.parent.mkdir(parents=True)
            embedded.write_bytes(self.contents[arch])
        self.module = {key.capitalize(): value for key, value in WINTUN_MODULE.items()}
        self.module["Dir"] = str(self.module_dir)
        write_json(self.plugin / "tool/wintun.json", self.lock)
        self.selection = canonicalize({})
        self.resolved = resolve_features(self.selection, "windows")
        self.pins = load_pins(self.plugin)
        self.bundle = self.root / "WindowsCore"
        self.bundle.mkdir()
        (self.bundle / WINDOWS_EXECUTABLE).write_bytes(pe("amd64"))
        (self.bundle / "LICENSE.wintun").write_bytes(self.contents["license"])
        self.cache = self.root / "cache"
        self.bootstrap = self.root / "bootstrap"
        self.bootstrap.mkdir()

    def identity(self, arch="amd64"):
        return build_core.build_identity(self.selection, [], self.pins, self.plugin,
                                         target="windows", windows_arch=arch)

    def published(self):
        identity = self.identity()
        fingerprint = build_core.canonical_hash(identity)
        capabilities = {"schema": 1, "version": self.pins["sing_box_version"], "fingerprint": fingerprint,
                        "features": self.resolved["features"], "windows_arch": "amd64"}
        metadata = {"schema": 1, "mode": "selectable-core", "target": "windows", "identity": identity,
                    "fingerprint": fingerprint, "features": self.resolved["features"],
                    "tags": self.resolved["tags"], "windows_arch": "amd64"}
        with mock.patch("windows_core.load_wintun_lock", return_value=self.lock):
            entry = build_core.publish_artifact(self.host, self.bundle, "windows", capabilities, metadata, {})
        build_core.update_receipt(self.host, capabilities, self.selection, {"windows": entry},
                                  receipt_path=build_core.WINDOWS_RECEIPT)
        receipt = json.loads((self.host / build_core.WINDOWS_RECEIPT).read_text())
        return entry, receipt, metadata

    def command(self, *arguments):
        return subprocess.run([sys.executable, "-I", "-X", "utf8", str(self.plugin / "tool/core_recipe.py"),
                               "--project-dir", str(self.host), "--target", "windows", *arguments],
                              capture_output=True, text=True, encoding="utf-8")


class WindowsIntegrity(WindowsFixture):
    def test_windows_and_mobile_receipts_do_not_replace_each_other(self):
        mobile = self.host / build_core.RECEIPT
        mobile.parent.mkdir(parents=True, exist_ok=True)
        mobile.write_text('{"artifacts": {"android": "existing mobile receipt"}}')
        original = mobile.read_bytes()
        _, receipt, _ = self.published()
        self.assertEqual(mobile.read_bytes(), original)
        windows = self.host / build_core.WINDOWS_RECEIPT
        windows_bytes = windows.read_bytes()
        build_core.update_receipt(self.host, {**receipt, "fingerprint": "new mobile identity"},
                                  self.selection, {})
        self.assertEqual(windows.read_bytes(), windows_bytes)
        self.assertEqual(build_core.receipt_for_target("windows"), build_core.WINDOWS_RECEIPT)
        self.assertEqual(build_core.receipt_for_target("apple"), build_core.RECEIPT)

    def test_helper_and_wintun_lock_and_architecture_contribute_to_identity(self):
        first = self.identity()
        self.assertNotEqual(first, self.identity("arm64"))
        for relative in ("native/windows/main.go", "native/windows/LICENSE.wintun",
                         *(f"native/mobile/{name}" for name in WINDOWS_SHARED_SOURCES),
                         "tool/windows_core.py", "tool/wintun.json"):
            path = self.plugin / relative
            original = path.read_bytes()
            path.write_bytes(original + b"\n")
            with self.subTest(path=relative):
                self.assertNotEqual(first, self.identity())
            path.write_bytes(original)

    def test_windows_source_overlay_is_restored_after_an_error(self):
        source = self.root / "source"
        (source / "cmd").mkdir(parents=True)
        registry = generate_registry(self.selection, target="windows")
        with self.assertRaisesRegex(RuntimeError, "interrupted"):
            with windows_overlay(source, registry, windows_sources(self.plugin), self.plugin):
                self.assertEqual((source / "cmd/flutter_windows/registry_selected.go").read_text(encoding="utf-8"), registry)
                for name in WINDOWS_SHARED_SOURCES:
                    raw = (self.plugin / "native/mobile" / name).read_bytes()
                    self.assertEqual((source / "cmd/flutter_windows" / name).read_bytes(),
                                     raw.replace(b"package libbox", b"package main"))
                    self.assertEqual(windows_sources(self.plugin)[f"mobile/{name}"], digest(raw))
                raise RuntimeError("interrupted")
        self.assertFalse((source / "cmd/flutter_windows").exists())
        with self.assertRaises(BuildError):
            with windows_overlay(source, registry, {"main.go": "a" * 64}, self.plugin):
                pass
        self.assertFalse((source / "cmd/flutter_windows").exists())

    def test_bundle_checks_pe_architecture_license_and_rejects_unused_sidecar(self):
        validate_windows_bundle(self.bundle, "amd64", self.lock)
        with self.assertRaisesRegex(BuildError, "architecture"):
            validate_windows_bundle(self.bundle, "arm64", self.lock)
        for name in (WINDOWS_EXECUTABLE, "LICENSE.wintun"):
            path = self.bundle / name
            original = path.read_bytes()
            path.write_bytes(b"modified")
            with self.subTest(name=name), self.assertRaises(BuildError):
                validate_windows_bundle(self.bundle, "amd64", self.lock)
            path.write_bytes(original)
        (self.bundle / "wintun.dll").write_bytes(self.contents["amd64"])
        with self.assertRaisesRegex(BuildError, "helper and embedded Wintun license only"):
            validate_windows_bundle(self.bundle, "amd64", self.lock)

    def test_actual_embedded_driver_matches_lock_and_provenance_has_no_cache_path(self):
        for arch in WINDOWS_ARCHES:
            run = mock.Mock(return_value=json.dumps(self.module))
            provenance = verify_embedded_wintun(Path("go.exe"), {"GOOS": "windows", "GOARCH": arch},
                                                self.root, arch, self.lock, run)
            self.assertEqual(run.call_args.args[0][1:], ["list", "-m", "-json", WINTUN_MODULE["path"]])
            self.assertEqual(provenance["arch"], arch)
            self.assertEqual(provenance["module"], WINTUN_MODULE)
            self.assertEqual(provenance["embedded_resource"], f"internal/wintun/{arch}/wintun.dll")
            self.assertEqual(provenance["dll_sha256"], self.lock["files"][arch]["sha256"])
            self.assertEqual(provenance["loading"], "embedded-memmod")
            self.assertNotIn(str(self.root), json.dumps(provenance))
            self.assertNotIn("Dir", provenance["module"])
        embedded = self.module_dir / "internal/wintun/amd64/wintun.dll"
        embedded.write_bytes(b"edited cached embedded resource")
        with self.assertRaisesRegex(BuildError, "embedded Wintun driver"):
            verify_embedded_wintun(Path("go"), {}, self.root, "amd64", self.lock,
                                  mock.Mock(return_value=json.dumps(self.module)))

    def test_embedded_driver_rejects_replacements_versions_sums_and_wrong_architecture(self):
        for edits in ({"Replace": {"Dir": "/private/replacement"}}, {"Version": "v0.0.0"},
                      {"Sum": "h1:unexpected"}, {"Path": "example.org/sing-tun"}, {"Main": True}):
            with self.subTest(edits=edits), self.assertRaisesRegex(BuildError, "pinned lock"):
                verify_embedded_wintun(Path("go"), {}, self.root, "amd64", self.lock,
                                      mock.Mock(return_value=json.dumps({**self.module, **edits})))
        path = self.module_dir / "internal/wintun/amd64/wintun.dll"
        path.write_bytes(self.contents["arm64"])
        changed = copy.deepcopy(self.lock)
        changed["files"]["amd64"]["sha256"] = digest(self.contents["arm64"])
        with self.assertRaises(BuildError):
            verify_embedded_wintun(Path("go"), {}, self.root, "amd64", changed,
                                  mock.Mock(return_value=json.dumps(self.module)))

    def test_builder_uses_windows_cgo_zero_and_package_owned_license_without_archive(self):
        output = self.root / "built"
        commands = []
        def run(command, *, cwd, env):
            commands.append(command)
            self.assertEqual((env["GOOS"], env["GOARCH"], env["CGO_ENABLED"]), ("windows", "arm64", "0"))
            Path(command[command.index("-o") + 1]).write_bytes(pe("arm64"))
        with mock.patch("native_common.download") as download:
            build_windows_bundle(Path("go.exe"), {}, self.root, output, "arm64", [],
                                 self.pins, self.lock, run, root=self.plugin)
            download.assert_not_called()
        self.assertEqual([command[1] for command in commands], ["build"])
        self.assertFalse((output / "wintun.dll").exists())
        validate_windows_bundle(output, "arm64", self.lock)
        license_path = self.plugin / "native/windows/LICENSE.wintun"
        license_path.write_bytes(b"changed license")
        with self.assertRaisesRegex(BuildError, "package-owned"), mock.patch("native_common.download") as download:
            build_windows_bundle(Path("go.exe"), {}, self.root, self.root / "bad", "arm64", [],
                                 self.pins, self.lock, run, root=self.plugin)
            download.assert_not_called()
        self.assertEqual(len(commands), 1)

    def test_cmake_prints_only_absolute_validated_bundle_and_checks_installed_pair(self):
        entry, receipt, metadata = self.published()
        artifact = build_core.host_path(self.host, entry["path"])
        result = self.command("--windows-arch", "amd64", "--print-artifact-path")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, str(artifact.resolve()) + "\n")
        self.assertEqual(result.stderr, "")
        result = self.command("--print-artifact-pair")
        self.assertEqual(result.returncode, 0, result.stderr)
        installed_digest, installed_path = result.stdout.rstrip("\n").split("\n", 1)
        self.assertEqual(installed_path, str(artifact.resolve()))
        self.assertEqual(self.command("--print-artifact-digest").stdout, installed_digest + "\n")
        self.assertEqual(installed_digest, build_core.canonical_hash(artifact_hashes(artifact)))
        installed = ("--installed-artifact", str(artifact), "--installed-sha256", installed_digest)
        self.assertEqual(self.command(*installed).returncode, 0)
        bad = self.command("--installed-artifact", str(artifact), "--installed-sha256", "0" * 64,
                           "--print-artifact-path")
        self.assertEqual(bad.returncode, 1)
        self.assertEqual(bad.stdout, "")
        self.assertIn("CMake", bad.stderr)
        bad = self.command("--windows-arch", "arm64", "--print-artifact-path")
        self.assertEqual(bad.returncode, 1)
        self.assertEqual(bad.stdout, "")
        self.assertIn("architecture", bad.stderr)
        (artifact / WINDOWS_EXECUTABLE).write_bytes(b"tampered")
        bad = self.command("--print-artifact-pair")
        self.assertEqual(bad.returncode, 1)
        self.assertEqual(bad.stdout, "")
        self.assertIn("changed after", bad.stderr)

    def test_windows_path_uses_one_full_combined_digest_and_reuses_only_intact_cache(self):
        entry, receipt, metadata = self.published()
        hashes = artifact_hashes(self.bundle)
        combined = build_core.canonical_hash({"recipe": receipt["fingerprint"], "artifact": hashes})
        self.assertEqual(entry["path"], f".dart_tool/sing_box_flutter/builds/windows/{combined}/WindowsCore")
        executable = (PureWindowsPath(r"C:\Users\developer\Projects\flutter_apps\sample_app") /
                      PureWindowsPath(entry["path"]) / WINDOWS_EXECUTABLE)
        self.assertEqual(len(str(executable)), 197)
        old = (PureWindowsPath(r"C:\Users\developer\Projects\flutter_apps\sample_app") /
               ".dart_tool/sing_box_flutter/builds" / receipt["fingerprint"] / "windows" /
               build_core.canonical_hash(hashes) / "WindowsCore" / WINDOWS_EXECUTABLE)
        self.assertEqual(len(str(old)), 262)
        recorded = json.loads(build_core.host_path(self.host, entry["metadata"]).read_text(encoding="utf-8"))
        self.assertEqual(recorded["fingerprint"], receipt["fingerprint"])
        self.assertEqual(recorded["identity"], metadata["identity"])
        self.assertEqual(recorded["artifact_sha256"], hashes)
        with mock.patch("build_core.shutil.copytree", wraps=shutil.copytree) as copying:
            self.assertEqual(self.published()[0], entry)
            copying.assert_not_called()
        artifact = build_core.host_path(self.host, entry["path"])
        (artifact / WINDOWS_EXECUTABLE).write_bytes(b"tampered cached helper")
        with self.assertRaisesRegex(BuildError, "modified"):
            self.published()

    def test_recipe_is_stale_after_helper_lock_or_pubspec_edits(self):
        self.published()
        for path in (self.plugin / "native/windows/main.go", self.plugin / "tool/wintun.json",
                     self.host / "pubspec.yaml"):
            original = path.read_bytes()
            path.write_bytes(original + b"\n")
            bad = self.command("--print-artifact-path")
            with self.subTest(path=path):
                self.assertEqual(bad.returncode, 1)
                self.assertEqual(bad.stdout, "")
                self.assertIn("stale", bad.stderr)
            path.write_bytes(original)


@unittest.skipUnless(shutil.which("cmake"), "CMake is not available")
class WindowsCMake(WindowsFixture):
    def configure(self, *, flutter_layout=False, install_prefix=None, late_install_prefix=None,
                  nested_host=False, discover_application=False, relative_install=False, bundle_subdir="",
                  directory_install=""):
        windows = self.plugin / "windows"
        windows.mkdir(exist_ok=True)
        shutil.copyfile(ROOT / "windows/CMakeLists.txt", windows / "CMakeLists.txt")
        shutil.copyfile(ROOT / "windows/install_resources.cmake.in", windows / "install_resources.cmake.in")
        shutil.copyfile(ROOT / "windows/verify_core_install.cmake.in", windows / "verify_core_install.cmake.in")
        (windows / "sing_box_flutter_plugin_c_api.cpp").write_text("// configure-only fixture\n", encoding="utf-8")
        (windows / "secure_core_launch.cpp").write_text("// configure-only fixture\n", encoding="utf-8")
        header = windows / "include/sing_box_flutter/sing_box_flutter_plugin_c_api.h"
        header.parent.mkdir(parents=True, exist_ok=True)
        header.write_text("// configure-only fixture\n", encoding="utf-8")
        harness = self.root / "cmake harness"
        harness.mkdir(exist_ok=True)
        text = '''cmake_minimum_required(VERSION 3.14)
project(core_harness LANGUAGES CXX)
function(apply_standard_settings target)
endfunction()
add_library(flutter INTERFACE)
add_library(flutter_wrapper_plugin INTERFACE)
'''
        if flutter_layout:
            text += '''set(BINARY_NAME fixture_runner)
file(WRITE "${CMAKE_CURRENT_BINARY_DIR}/runner.cpp" "int main() { return 0; }")
add_executable(${BINARY_NAME} "${CMAKE_CURRENT_BINARY_DIR}/runner.cpp")
set_target_properties(${BINARY_NAME} PROPERTIES RUNTIME_OUTPUT_DIRECTORY "${CMAKE_CURRENT_BINARY_DIR}/runner")
'''
        text += '''add_subdirectory("${PLUGIN_DIR}/windows" plugin)
list(LENGTH sing_box_flutter_bundled_libraries bundle_count)
list(GET sing_box_flutter_bundled_libraries 0 bundle_path)
file(WRITE "${CMAKE_BINARY_DIR}/bundled.txt" "${bundle_count}\\n${bundle_path}")
'''
        if flutter_layout:
            # Match Flutter's ordering: the default prefix is assigned only
            # after generated_plugins.cmake has added the plugin subdirectory.
            text += '''if(DEFINED LATE_INSTALL_PREFIX)
  set(CMAKE_INSTALL_PREFIX "${LATE_INSTALL_PREFIX}")
elseif(CMAKE_INSTALL_PREFIX_INITIALIZED_TO_DEFAULT)
  set(CMAKE_INSTALL_PREFIX "$<TARGET_FILE_DIR:${BINARY_NAME}>" CACHE PATH "..." FORCE)
endif()
file(GENERATE OUTPUT "${CMAKE_CURRENT_BINARY_DIR}/host-prefix-$<CONFIG>.txt" CONTENT "${CMAKE_INSTALL_PREFIX}")
'''
            destination = (bundle_subdir or ".") if relative_install else "${CMAKE_INSTALL_PREFIX}"
            if bundle_subdir and not relative_install:
                destination += "/" + bundle_subdir
            runner_destination = destination
            if directory_install:
                text += '''get_filename_component(CORE_BUNDLE_DIRECTORY "${bundle_path}" DIRECTORY)
get_filename_component(CORE_BUNDLE_NAME "${CORE_BUNDLE_DIRECTORY}" NAME)
get_filename_component(CORE_BUNDLE_PARENT "${CORE_BUNDLE_DIRECTORY}" DIRECTORY)
'''
                if directory_install in ("named", "parent"):
                    runner_destination += "/${CORE_BUNDLE_NAME}"
            text += 'install(TARGETS ${BINARY_NAME} RUNTIME DESTINATION "' + runner_destination + '" COMPONENT Runtime)\n'
            if directory_install:
                source = {"contents": "${CORE_BUNDLE_DIRECTORY}/", "named": "${CORE_BUNDLE_DIRECTORY}",
                          "parent": "${CORE_BUNDLE_PARENT}/"}[directory_install]
                text += 'install(DIRECTORY "' + source + '" DESTINATION "' + destination + '" COMPONENT Runtime)\n'
            else:
                text += 'install(FILES ${sing_box_flutter_bundled_libraries} DESTINATION "' + destination + '" COMPONENT Runtime)\n'
        if nested_host:
            host_source = self.host / "windows"
            host_source.mkdir(exist_ok=True)
            (host_source / "CMakeLists.txt").write_text(text, encoding="utf-8")
            (harness / "CMakeLists.txt").write_text('''cmake_minimum_required(VERSION 3.19)
project(core_superbuild LANGUAGES CXX)
set(CMAKE_INSTALL_PREFIX "${SUPERBUILD_INSTALL_PREFIX}")
add_subdirectory("${HOST_SOURCE_DIR}" host)
''', encoding="utf-8")
        else:
            (harness / "CMakeLists.txt").write_text(text, encoding="utf-8")
        self.cmake_build = self.root / "cmake output"
        command = ["cmake", "-S", str(harness), "-B", str(self.cmake_build),
                   f"-DPLUGIN_DIR={self.plugin}", "-DFLUTTER_TARGET_PLATFORM=windows-x64",
                   f"-DPython3_EXECUTABLE={sys.executable}"]
        if not discover_application:
            command += [f"-DFLUTTER_APPLICATION_PATH={self.host}"]
        if nested_host:
            command += [f"-DHOST_SOURCE_DIR={host_source.as_posix()}",
                        f"-DSUPERBUILD_INSTALL_PREFIX={(self.root / 'superbuild install').as_posix()}"]
        if flutter_layout and os.name != "nt":
            if not shutil.which("ninja"):
                self.skipTest("Ninja is required for the multi-config Flutter layout")
            command += ["-G", "Ninja Multi-Config"]
        # CMake variables without a PATH cache type retain Windows backslashes,
        # which would become escape sequences in generated install scripts.
        if install_prefix is not None:
            command += [f"-DCMAKE_INSTALL_PREFIX={install_prefix.as_posix()}"]
        if late_install_prefix is not None:
            command += [f"-DLATE_INSTALL_PREFIX={late_install_prefix.as_posix()}"]
        return subprocess.run(command, capture_output=True, text=True, encoding="utf-8")

    def verify_installed(self):
        return subprocess.run(["cmake", "--build", str(self.cmake_build), "--target", "sing_box_flutter_verify_core"],
                              capture_output=True, text=True, encoding="utf-8")

    def test_atomic_pair_and_all_guard_support_unicode_spaces_and_semicolons(self):
        renamed = self.host.with_name(self.host.name + "; host")
        self.host.rename(renamed)
        self.host = renamed
        entry, _, _ = self.published()
        result = self.configure()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        artifact = build_core.host_path(self.host, entry["path"])
        self.assertEqual((self.cmake_build / "bundled.txt").read_text(encoding="utf-8"),
                         "1\n" + (artifact / WINDOWS_EXECUTABLE).as_posix())
        result = self.verify_installed()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        (artifact / WINDOWS_EXECUTABLE).write_bytes(b"tampered after configuration")
        result = self.verify_installed()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("changed after", result.stdout + result.stderr)

    def test_install_uses_verified_license_when_cached_legal_copy_is_modified(self):
        entry, _, _ = self.published()
        artifact = build_core.host_path(self.host, entry["path"])
        expected = (artifact / "LICENSE.wintun").read_bytes()
        legal = artifact.parent / "Legal"
        legal.mkdir()
        (legal / "LICENSE.wintun").write_bytes(b"unverified cached legal copy\n")
        (legal / "NOTICE").write_text("other required notice\n", encoding="utf-8")
        result = self.configure()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        result = self.verify_installed()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        installed = self.root / "installed bundle"
        result = subprocess.run(["cmake", "--install", str(self.cmake_build), "--component", "Runtime",
                                 "--prefix", str(installed)], capture_output=True, text=True, encoding="utf-8")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((installed / "sing_box_flutter/Legal/LICENSE.wintun").read_bytes(), expected)
        self.assertEqual((installed / "sing_box_flutter/Legal/NOTICE").read_text(encoding="utf-8"),
                         "other required notice\n")
        self.assertNotEqual((legal / "LICENSE.wintun").read_bytes(), expected)

    def test_install_rejects_tampered_or_reselected_core_before_copying(self):
        for mutation in ("tamper", "reselect", "pubspec"):
            with self.subTest(mutation=mutation):
                entry, _, _ = self.published()
                result = self.configure(flutter_layout=True, install_prefix=self.root / "configured install")
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                result = subprocess.run(["cmake", "--build", str(self.cmake_build),
                                         "--config", "Release", "--target", "fixture_runner"],
                                        capture_output=True, text=True, encoding="utf-8")
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                artifact = build_core.host_path(self.host, entry["path"])
                helper = artifact / WINDOWS_EXECUTABLE
                before = helper.read_bytes()
                manifest = self.host / "pubspec.yaml"
                pubspec_before = manifest.read_bytes()
                try:
                    if mutation == "tamper":
                        helper.write_bytes(before + b"tampered")
                    elif mutation == "reselect":
                        (self.bundle / WINDOWS_EXECUTABLE).write_bytes(pe("amd64") + b"new verified build")
                        self.published()
                    else:
                        manifest.write_bytes(pubspec_before + b"# selection changed after configure\n")
                    installed = self.root / ("rejected " + mutation)
                    result = subprocess.run(["cmake", "--install", str(self.cmake_build),
                                             "--config", "Release", "--component", "Runtime",
                                             "--prefix", str(installed)],
                                            capture_output=True, text=True, encoding="utf-8")
                    self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn("install-time verification", result.stdout + result.stderr)
                    self.assertFalse((self.root / "configured install" / WINDOWS_EXECUTABLE).exists())
                    self.assertFalse((installed / WINDOWS_EXECUTABLE).exists())
                finally:
                    helper.write_bytes(before)
                    manifest.write_bytes(pubspec_before)
                    (self.bundle / WINDOWS_EXECUTABLE).write_bytes(pe("amd64"))
                    self.published()

    def assert_flutter_install(self, install_prefix=None, late_install_prefix=None,
                               nested_host=False, discover_application=False,
                               relative_install=False, override_prefix=None, bundle_subdir="",
                               directory_install="", lower_config=False):
        entry, _, _ = self.published()
        artifact = build_core.host_path(self.host, entry["path"])
        legal = artifact.parent / "Legal"
        legal.mkdir(exist_ok=True)
        (legal / "NOTICE").write_text("required notice\n", encoding="utf-8")
        for configuration in ("Debug", "Release"):
            with self.subTest(configuration=configuration, prefix=install_prefix, late_prefix=late_install_prefix):
                # Calling configure twice also covers the cached generator
                # expression prefix used on Flutter's subsequent builds.
                result = self.configure(flutter_layout=True, install_prefix=install_prefix,
                                        late_install_prefix=late_install_prefix, nested_host=nested_host,
                                        discover_application=discover_application, relative_install=relative_install,
                                        bundle_subdir=bundle_subdir, directory_install=directory_install)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                result = subprocess.run(["cmake", "--build", str(self.cmake_build),
                                         "--config", configuration, "--target", "fixture_runner",
                                         "sing_box_flutter_verify_core"], cwd=self.root,
                                        capture_output=True, text=True, encoding="utf-8")
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                install_configuration = configuration.lower() if lower_config else configuration
                install_command = ["cmake", "--install", str(self.cmake_build),
                                   "--config", install_configuration, "--component", "Runtime"]
                if override_prefix is not None:
                    install_command += ["--prefix", str(override_prefix)]
                result = subprocess.run(install_command, cwd=self.root,
                                        capture_output=True, text=True, encoding="utf-8")
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                runner_build = self.cmake_build / "host" if nested_host else self.cmake_build
                # Read the prefix chosen by the host, not any plugin output.
                # A superbuild may change the inherited default on reconfigure.
                installed = Path((runner_build / f"host-prefix-{configuration}.txt").read_text(encoding="utf-8"))
                chosen = late_install_prefix or install_prefix
                if chosen is not None:
                    self.assertEqual(installed, chosen)
                elif not nested_host:
                    self.assertEqual(installed, runner_build / "runner" / configuration)
                if relative_install and override_prefix is not None:
                    installed = override_prefix
                if bundle_subdir:
                    installed /= bundle_subdir
                if directory_install in ("named", "parent"):
                    installed /= artifact.name
                self.assertEqual((installed / WINDOWS_EXECUTABLE).read_bytes(), self.contents["amd64"])
                resources = installed / "sing_box_flutter"
                for name in ("core-capabilities.json", "native-build.json"):
                    self.assertTrue((resources / name).is_file(), name)
                self.assertEqual((resources / "Legal/LICENSE.wintun").read_bytes(), self.contents["license"])
                self.assertEqual((resources / "Legal/NOTICE").read_text(encoding="utf-8"), "required notice\n")

    def test_flutter_multi_config_install_expands_default_and_cached_prefix(self):
        self.assert_flutter_install()

    def test_flutter_multi_config_install_honors_custom_prefix(self):
        self.assert_flutter_install(self.root / "custom install bundle")

    def test_flutter_multi_config_install_honors_prefix_set_after_plugin(self):
        self.assert_flutter_install(late_install_prefix=self.root / "late custom install bundle")

    def test_nested_flutter_host_install_discovers_application_and_default_prefix(self):
        self.assert_flutter_install(nested_host=True, discover_application=True)

    def test_nested_flutter_host_install_honors_local_prefix_set_after_plugin(self):
        self.assert_flutter_install(nested_host=True,
                                    late_install_prefix=self.root / "nested host install bundle")

    def test_relative_runner_install_honors_override_prefix(self):
        self.assert_flutter_install(install_prefix=self.root / "configured bundle",
                                    relative_install=True, override_prefix=self.root / "staged bundle; with spaces")

    def test_nested_relative_runner_install_honors_override_prefix(self):
        self.assert_flutter_install(nested_host=True, relative_install=True,
                                    late_install_prefix=self.root / "local configured bundle",
                                    override_prefix=self.root / "nested staged bundle")

    def test_absolute_runner_install_keeps_resources_with_helper_under_override_prefix(self):
        self.assert_flutter_install(late_install_prefix=self.root / "absolute host bundle",
                                    override_prefix=self.root / "unused override bundle")

    def test_relative_runner_install_preserves_semicolons_in_subdirectory(self):
        self.assert_flutter_install(install_prefix=self.root / "configured bundle", relative_install=True,
                                    override_prefix=self.root / "staged root; with spaces", bundle_subdir="bin;tools")

    def test_nested_absolute_runner_install_preserves_semicolons_in_subdirectory(self):
        self.assert_flutter_install(nested_host=True, late_install_prefix=self.root / "absolute bundle",
                                    override_prefix=self.root / "unused staging", bundle_subdir="nested;bin")

    def test_install_configuration_case_is_ignored(self):
        self.assert_flutter_install(install_prefix=self.root / "case insensitive config", lower_config=True)

    def test_directory_installer_contents_follows_helper(self):
        self.assert_flutter_install(relative_install=True, override_prefix=self.root / "directory staged bundle",
                                    bundle_subdir="bin;tools", directory_install="contents")

    def test_directory_installer_named_source_follows_helper(self):
        self.assert_flutter_install(nested_host=True, late_install_prefix=self.root / "named directory bundle",
                                    override_prefix=self.root / "unused stage", bundle_subdir="bin",
                                    directory_install="named")

    def test_directory_installer_parent_contents_follows_nested_helper(self):
        self.assert_flutter_install(install_prefix=self.root / "parent configured bundle", relative_install=True,
                                    override_prefix=self.root / "parent staged bundle", bundle_subdir="bin;parent",
                                    directory_install="parent")

    def test_pair_parser_keeps_windows_backslashes_semicolons_and_crlf(self):
        script = self.plugin / "tool/core_recipe.py"
        script.write_text('''import sys
assert sys.flags.isolated and sys.flags.utf8_mode
path = "C:/Users/应用 host; name/core/WindowsCore"
digest = "a" * 64
if "--print-artifact-pair" in sys.argv:
    assert "--print-artifact-path" not in sys.argv and "--print-artifact-digest" not in sys.argv
    sys.stdout.buffer.write((digest + "\\r\\n" + path.replace("/", chr(92)) + "\\r\\n").encode("utf-8"))
else:
    assert sys.argv[sys.argv.index("--installed-artifact") + 1] == path, sys.argv
    assert sys.argv[sys.argv.index("--installed-sha256") + 1] == digest, sys.argv
''', encoding="utf-8")
        result = self.configure()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.cmake_build / "bundled.txt").read_text(encoding="utf-8"),
                         "1\nC:/Users/应用 host; name/core/WindowsCore/" + WINDOWS_EXECUTABLE)
        result = self.verify_installed()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


class WindowsDependencyOrder(WindowsFixture):
    def test_resolve_host_and_target_test_graphs_before_verify_without_compiling(self):
        run = mock.Mock()
        env = prepare_windows_dependencies(Path("go"), {"GOOS": "wrong", "GOARCH": "386", "CGO_ENABLED": "1"},
                                            self.root, "arm64", ["with_gvisor"], run)
        calls = run.call_args_list
        expected = [Path("go"), "list", "-deps", "-test", "-tags", "with_gvisor", "./cmd/flutter_windows"]
        self.assertEqual(calls[0].args[0], expected)
        self.assertEqual(calls[1].args[0], expected)
        self.assertNotIn("GOOS", calls[0].kwargs["env"])
        self.assertNotIn("GOARCH", calls[0].kwargs["env"])
        self.assertEqual(calls[0].kwargs["env"]["CGO_ENABLED"], "0")
        self.assertEqual((env["GOOS"], env["GOARCH"], env["CGO_ENABLED"]), ("windows", "arm64", "0"))
        self.assertEqual(calls[1].kwargs["env"], env)
        self.assertEqual(calls[2].args[0], [Path("go"), "mod", "verify"])
        self.assertEqual(len(calls), 3)

    def test_all_dependency_and_driver_failures_precede_tests_and_compilation(self):
        self.cache.mkdir()
        source = self.root / "selected source"
        source.mkdir()
        (source / "LICENSE").write_text("source license\n", encoding="utf-8")
        identity = self.identity()
        fingerprint = build_core.canonical_hash(identity)
        for failure in ("host-graph", "windows-graph", "verify", "driver", None):
            commands = []
            def run(command, *, cwd, env, capture=False):
                commands.append((command, env))
                if command[1:4] == ["list", "-deps", "-test"]:
                    stage = "windows-graph" if env.get("GOOS") == "windows" else "host-graph"
                elif command[1:3] == ["mod", "verify"]:
                    stage = "verify"
                elif command[1:4] == ["list", "-m", "-json"]:
                    return json.dumps({**self.module, **({"Sum": "h1:altered"} if failure == "driver" else {})})
                else:
                    stage = command[1]
                if failure == stage:
                    raise subprocess.CalledProcessError(1, command)
                if command[1] == "build":
                    Path(command[command.index("-o") + 1]).write_bytes(pe("amd64"))
                return "selected graph\n"
            with self.subTest(failure=failure), contextlib.ExitStack() as stack:
                stack.enter_context(mock.patch("build_core.run", side_effect=run))
                stack.enter_context(mock.patch("windows_core.windows_overlay", side_effect=lambda s, r, e: windows_overlay(s, r, e, self.plugin)))
                stack.enter_context(mock.patch("build_core.build_windows_bundle", side_effect=lambda *args: build_windows_bundle(*args, root=self.plugin)))
                stack.enter_context(mock.patch("build_core.legal_files", return_value={}))
                stack.enter_context(mock.patch("windows_core.load_wintun_lock", return_value=self.lock))
                def build():
                    return build_core.build_selected_windows(self.host, source, Path("go"), {}, {}, self.pins,
                                                             self.resolved, identity, fingerprint, [], self.cache)
                if failure is not None:
                    with self.assertRaises((BuildError, subprocess.CalledProcessError)):
                        build()
                    self.assertFalse(any(command[1] in ("test", "build") for command, _ in commands))
                else:
                    artifacts, _ = build()
                    self.assertIn("windows", artifacts)
                    self.assertEqual([command[1] for command, _ in commands], ["list", "list", "mod", "list", "test", "build"])
                    self.assertEqual(commands[2][0][1:], ["mod", "verify"])
                self.assertFalse(any(command[1:3] == ["mod", "download"] for command, _ in commands))
                self.assertFalse(any("-compiled" in command or "-export" in command for command, _ in commands))
                self.assertFalse((source / "cmd/flutter_windows").exists())

    def test_wgc_resolves_only_selected_graphs_and_restores_overlay_on_failure(self):
        self.cache.mkdir()
        source = self.root / "selected source"
        registry = source / "include/registry.go"
        builder = source / "cmd/internal/build_libbox/main.go"
        registry.parent.mkdir(parents=True)
        builder.parent.mkdir(parents=True)
        # Match the pinned upstream Go sources, independent of host text defaults.
        registry.write_text("package include\nfunc EndpointRegistry() {\n\tregisterTailscaleEndpoint(registry)\n}\n",
                            encoding="utf-8", newline="\n")
        builder.write_text('package main\nfunc init() { sharedTags = append(sharedTags, "with_gvisor") }\n',
                           encoding="utf-8", newline="\n")
        (source / "go.mod").write_text("module github.com/sagernet/sing-box\n")
        (source / "go.sum").write_text("")
        (source / "LICENSE").write_text("source license\n")
        originals = {p: p.read_bytes() for p in (registry, builder, source / "go.mod", source / "go.sum")}
        self.selection = canonicalize({"endpoints": ["wgc"]})
        resolved = resolve_features(self.selection, "windows")
        identity = self.identity("arm64")
        fingerprint = build_core.canonical_hash(identity)
        env = {"GOFLAGS": "-mod=readonly -trimpath -buildvcs=false", "GOWORK": "off", "CGO_ENABLED": "1"}
        original_env = dict(env)
        expected_graph = [Path("go"), "list", "-deps", "-test", "-tags",
                          ",".join(resolved["tags"]), "./cmd/flutter_windows"]
        for failure in ("host-graph", "windows-graph", "verify", "driver", "test", "build", None):
            commands = []
            def run(command, *, cwd, env, capture=False):
                self.assertEqual(cwd, source)
                commands.append((command, dict(env)))
                if command[1:4] == ["list", "-deps", "-test"]:
                    self.assertEqual(command, expected_graph)
                    self.assertTrue(capture)
                    self.assertEqual(env["GOFLAGS"], "-mod=mod -trimpath -buildvcs=false")
                    self.assertEqual(env["CGO_ENABLED"], "0")
                    if env.get("GOOS") == "windows":
                        self.assertEqual(env["GOARCH"], "arm64")
                        stage = "windows-graph"
                    else:
                        self.assertNotIn("GOOS", env)
                        self.assertNotIn("GOARCH", env)
                        stage = "host-graph"
                    # Selected resolution may update go.mod, but it must roll back.
                    (source / "go.mod").write_text("module github.com/sagernet/sing-box\n// resolved selected dependencies\n")
                else:
                    self.assertEqual(env["GOFLAGS"], original_env["GOFLAGS"])
                    if command[1:3] == ["mod", "verify"]:
                        stage = "verify"
                    elif command[1:4] == ["list", "-m", "-json"]:
                        return json.dumps({**self.module, **({"Sum": "h1:altered"} if failure == "driver" else {})})
                    else:
                        stage = command[1]
                if failure == stage:
                    raise subprocess.CalledProcessError(1, command)
                if command[1] in ("test", "build"):
                    self.assertTrue(any(cmd[1:3] == ["mod", "verify"] for cmd, _ in commands))
                    self.assertEqual(command[command.index("-tags") + 1], ",".join(resolved["tags"]))
                if command[1] == "build":
                    Path(command[command.index("-o") + 1]).write_bytes(pe("arm64"))
                return "selected graph\n"

            with self.subTest(failure=failure), contextlib.ExitStack() as stack:
                stack.enter_context(mock.patch("build_core.run", side_effect=run))
                stack.enter_context(mock.patch("windows_core.windows_overlay", side_effect=lambda s, r, e: windows_overlay(s, r, e, self.plugin)))
                stack.enter_context(mock.patch("build_core.wgc_overlay", side_effect=lambda *args, **kwargs: wgc_overlay(*args, root=self.plugin, **kwargs)))
                stack.enter_context(mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)))
                stack.enter_context(mock.patch("build_core.retain_wgc_license", return_value=None))
                stack.enter_context(mock.patch("build_core.build_windows_bundle", side_effect=lambda *args: build_windows_bundle(*args, root=self.plugin)))
                stack.enter_context(mock.patch("build_core.legal_files", return_value={}))
                stack.enter_context(mock.patch("windows_core.load_wintun_lock", return_value=self.lock))
                prepare = stack.enter_context(mock.patch("build_core.prepare_windows_dependencies"))
                def build():
                    return build_core.build_selected_windows(self.host, source, Path("go"), env, {}, self.pins,
                                                             resolved, identity, fingerprint, [], self.cache)
                if failure is None:
                    artifacts, capabilities = build()
                    metadata = json.loads(build_core.host_path(self.host, artifacts["windows"]["metadata"]).read_text())
                    self.assertEqual(capabilities["features"], resolved["features"])
                    self.assertEqual(metadata["tags"], resolved["tags"])
                    self.assertIsNone(metadata["tailscale_dependency"])
                    self.assertIn("go_sum_sha256", metadata["wgc"])
                    self.assertEqual([cmd[1] for cmd, _ in commands], ["mod", "list", "list", "mod", "list", "test", "build"])
                else:
                    with self.assertRaises((BuildError, subprocess.CalledProcessError)):
                        build()
                    if failure not in ("test", "build"):
                        self.assertFalse(any(cmd[1] in ("test", "build") for cmd, _ in commands))
                prepare.assert_not_called()
                self.assertFalse(any(cmd[1:3] in (["mod", "tidy"], ["mod", "download"]) for cmd, _ in commands))
                self.assertFalse(any("-compiled" in cmd or "-export" in cmd for cmd, _ in commands))
                for path, original in originals.items():
                    self.assertEqual(path.read_bytes(), original)
                for relative in ("cmd/flutter_windows", "include/wgc_flutter.go", ".wgc-inputs"):
                    self.assertFalse((source / relative).exists())
                self.assertEqual(env, original_env)

    def test_tailscale_metadata_follows_resolved_tags_for_all_selections(self):
        self.cache.mkdir()
        source = self.root / "selected source"
        source.mkdir()
        (source / "LICENSE").write_text("source license\n")
        for selection in ({}, {"endpoints": ["tailscale"]}, {"dns": ["tailscale"]},
                          {"certificate_providers": ["tailscale"]}, {"services": ["derp"]}):
            with self.subTest(selection=selection), contextlib.ExitStack() as stack:
                self.selection = canonicalize(selection)
                resolved = resolve_features(self.selection, "windows")
                identity = self.identity()
                fingerprint = build_core.canonical_hash(identity)
                dependency = {"version": "v1.99.0", "sum": "authenticated dependency fixture"}
                tailscale = stack.enter_context(mock.patch("build_core.resolved_tailscale_dependency", return_value=dependency))
                stack.enter_context(mock.patch("windows_core.windows_overlay", side_effect=lambda s, r, e: windows_overlay(s, r, e, self.plugin)))
                stack.enter_context(mock.patch("build_core.prepare_windows_dependencies", return_value={}))
                stack.enter_context(mock.patch("build_core.verify_embedded_wintun", return_value={}))
                stack.enter_context(mock.patch("build_core.run"))
                stack.enter_context(mock.patch("build_core.build_windows_bundle"))
                stack.enter_context(mock.patch("build_core.legal_files", return_value={}))
                publish = stack.enter_context(mock.patch("build_core.publish_artifact", return_value={}))
                _, capabilities = build_core.build_selected_windows(self.host, source, Path("go"), {}, {}, self.pins,
                                                                     resolved, identity, fingerprint, [], self.cache)
                selected = "with_tailscale" in resolved["tags"]
                self.assertEqual(tailscale.call_count, int(selected))
                metadata = publish.call_args.args[4]
                self.assertEqual(metadata["tailscale_dependency"], dependency if selected else None)
                self.assertEqual(metadata["features"], resolved["features"])
                self.assertEqual(metadata["tags"], resolved["tags"])
                self.assertEqual(capabilities["features"], resolved["features"])


class WindowsChangeGate(unittest.TestCase):
    BASE, HEAD = "a" * 40, "b" * 40

    def gate(self, names, event="pull_request", *, failure=None):
        payload = ({"pull_request": {"base": {"sha": self.BASE}, "head": {"sha": self.HEAD}}}
                   if event == "pull_request" else {"before": self.BASE, "after": self.HEAD})
        def run(command, **kwargs):
            if failure is not None:
                raise failure
            if command[1] == "rev-parse":
                return types.SimpleNamespace(stdout=command[-1].split("^", 1)[0] + "\n")
            self.assertIn("--no-renames", command)
            self.assertEqual(command[-2] if event == "pull_request" else command[-3:-1],
                             f"{self.BASE}...{self.HEAD}" if event == "pull_request" else [self.BASE, self.HEAD])
            return types.SimpleNamespace(stdout="\0".join(names) + "\0")
        return needs_windows_build(event, payload, run=run)

    def test_docs_only_skip_but_native_and_packaging_inputs_build(self):
        for event in ("pull_request", "push"):
            self.assertFalse(self.gate(["README.md", "doc/windows-setup.md", "example/README.md"], event))
            for name in ("tool/core_recipe.py", "native/mobile/capabilities.go", "native/windows/LICENSE.wintun",
                         "windows/CMakeLists.txt", "lib/sing_box_flutter_windows.dart", "pubspec.yaml",
                         "example/windows/runner/main.cpp", "example/lib/应用.dart", ".github/workflows/check.yml"):
                with self.subTest(event=event, name=name):
                    self.assertTrue(self.gate([name], event))

    def test_unclassified_inputs_build_instead_of_being_treated_as_docs(self):
        for event in ("pull_request", "push"):
            for name in (".gitattributes", ".gitignore", ".pubignore", "LICENSE",
                         "THIRD_PARTY_NOTICES.md", "future/build.json", "example/tool/codegen.dart",
                         ".github/workflows/new-check.yml"):
                with self.subTest(event=event, name=name):
                    self.assertTrue(self.gate(["README.md", name], event))

    def test_dispatch_missing_history_and_invalid_event_data_fail_closed(self):
        self.assertTrue(needs_windows_build("workflow_dispatch", {}))
        self.assertTrue(needs_windows_build("pull_request", {}))
        self.assertTrue(needs_windows_build("push", {"before": "0" * 40, "after": self.HEAD}))
        self.assertTrue(needs_windows_build("push", {"before": "--bad", "after": self.HEAD}))
        self.assertTrue(self.gate([], failure=subprocess.CalledProcessError(1, "git")))
        self.assertTrue(self.gate([], failure=FileNotFoundError("git")))
        invalid_revision = mock.Mock(return_value=types.SimpleNamespace(stdout="c" * 40))
        self.assertTrue(needs_windows_build("push", {"before": self.BASE, "after": self.HEAD}, run=invalid_revision))


class PortableLocks(unittest.TestCase):
    @unittest.skipIf(os.name == "nt", "POSIX lock contention test")
    def test_nonblocking_lock_releases_after_an_error(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "lock"
            with self.assertRaisesRegex(RuntimeError, "interrupted"):
                with file_lock(path, "busy"):
                    with self.assertRaisesRegex(BuildError, "busy"):
                        with file_lock(path, "busy"):
                            pass
                    raise RuntimeError("interrupted")
            with file_lock(path, "busy"):
                pass

    @unittest.skipIf(os.name == "nt", "Mock Windows byte lock exercised from POSIX")
    def test_windows_locks_byte_zero_and_unlocks_on_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "lock"
            operations = []
            def locking(fd, operation, size):
                operations.append((operation, size, os.lseek(fd, 0, os.SEEK_CUR)))
            windows = types.SimpleNamespace(LK_NBLCK=1, LK_UNLCK=2, locking=locking)
            with mock.patch.dict(sys.modules, {"msvcrt": windows}), mock.patch("native_common.os.name", "nt"):
                with self.assertRaisesRegex(RuntimeError, "interrupted"):
                    with file_lock(path, "busy"):
                        raise RuntimeError("interrupted")
            self.assertEqual(operations, [(1, 1, 0), (2, 1, 0)])
            self.assertEqual(path.read_bytes(), b"\0")


if __name__ == "__main__":
    unittest.main()
