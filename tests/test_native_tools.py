"""Focused tests for native input integrity; no SDK, network or Go is required."""
import io
import json
import os
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest import mock
import zipfile

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
from native_common import (ARTIFACTS, BuildError, ROOT, REQUIRED_TAGS, archive_name, artifact_hashes,
                           build_directory, load_pins, parse_manifest, pin_identity, safe_extract,
                           sha256, validate_metadata)
from build_libbox import bootstrap_go, go_environment, mobile_profile_bytes, resolved_tailscale_dependency, verify_dependency_toolchain
import install_libbox


class PinTests(unittest.TestCase):
    def test_core_pins_and_dependency_matching_fork(self):
        self.assertEqual(pin_identity(load_pins()), {
            "sing_box_version": "1.14.4",
            "sing_box_revision": "c005f35300ce76427dc285481ec5695f5b179cd2",
            "tailscale_go_revision": "63ae404c8203317fd3c82d972e5dc8f0fcb425cb",
            "gomobile_version": "v0.1.12",
            "wgc_version": "v0.2.1-0.20261107180014-c3ca5bb852e4",
            "wgc_source_sha256": pin_identity(load_pins())["wgc_source_sha256"],
            "wgc_overlay_sha256": sha256(ROOT / "tool/wgc_overlay.py"),
        })

    def test_revision_and_checksum_lock_must_agree(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "tool").mkdir()
            pins = load_pins()
            pins["tailscale_go_revision"] = "1" * 40
            (root / "native-core.json").write_text(json.dumps(pins))
            (root / "tool/tailscale-go.json").write_text((ROOT / "tool/tailscale-go.json").read_text())
            with self.assertRaisesRegex(BuildError, "checksum lock"):
                load_pins(root)

    def test_release_cannot_change_core_or_mobile_features(self):
        pins = load_pins()
        profile = json.loads((ROOT / "tool/mobile-profile.json").read_text())
        metadata = {"schema": 1, "target": "apple", "pins": pin_identity(pins),
                    "tags": [*REQUIRED_TAGS, "with_low_memory"],
                    "tailscale_dependency": {"toolchain_revision": pins["tailscale_go_revision"]},
                    "mobile_profile": {"profile_sha256": sha256(ROOT / "tool/mobile-profile.json"),
                                       "upstream_builder_sha256": profile["upstream_builder_sha256"],
                                       "removed_tags": profile["drop_tags"]}}
        metadata["wgc"] = {"source_sha256": metadata["pins"]["wgc_source_sha256"],
                           "overlay_sha256": metadata["pins"]["wgc_overlay_sha256"], "extensions": []}
        validate_metadata(metadata, pins, "apple")
        metadata["pins"]["sing_box_revision"] = "f" * 40
        with self.assertRaises(BuildError):
            validate_metadata(metadata, pins, "apple")
        metadata["pins"] = pin_identity(pins)
        metadata["tags"].remove("with_low_memory")
        with self.assertRaises(BuildError):
            validate_metadata(metadata, pins, "apple")

    def test_mobile_profile_checks_hash_and_removes_only_optional_tags(self):
        import hashlib
        content = b'sharedTags = append(sharedTags, "with_gvisor", "with_tailscale", "with_naive_outbound", "with_usbip", "with_openvpn", "with_openconnect")\nlegacyTags := filterTags(sharedTags, "with_naive_outbound")\n'
        profile = {"upstream_builder_sha256": hashlib.sha256(content).hexdigest(),
                   "drop_tags": ["with_naive_outbound", "with_usbip", "with_openvpn", "with_openconnect"]}
        patched = mobile_profile_bytes(content, profile)
        self.assertEqual(patched, b'sharedTags = append(sharedTags, "with_gvisor", "with_tailscale")\nlegacyTags := filterTags(sharedTags, "with_naive_outbound")\n')
        with self.assertRaises(BuildError):
            mobile_profile_bytes(content + b"// modified", profile)
        profile["drop_tags"] = ["with_tailscale"]
        with self.assertRaises(BuildError):
            mobile_profile_bytes(content, profile)

    def test_sha_manifest_rejects_paths_duplicates_and_missing_hash(self):
        good = "a" * 64 + "  libbox-1.14.4-apple.tar.gz\n"
        self.assertEqual(parse_manifest(good)["libbox-1.14.4-apple.tar.gz"], "a" * 64)
        for text in (good + good, "bad  a.tar.gz", "a" * 64 + "  ../bad.tar.gz", ""):
            with self.subTest(text=text), self.assertRaises(BuildError):
                parse_manifest(text)


class SafeExtractionTests(unittest.TestCase):
    def extract(self, entries):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        archive = root / "input.tar.gz"
        with tarfile.open(archive, "w:gz") as bundle:
            for name, kind, data in entries:
                info = tarfile.TarInfo(name)
                if kind == "file":
                    content = data.encode()
                    info.size = len(content)
                    bundle.addfile(info, io.BytesIO(content))
                else:
                    info.type = tarfile.SYMTYPE if kind == "symlink" else tarfile.LNKTYPE
                    info.linkname = data
                    bundle.addfile(info)
        destination = root / "out"
        safe_extract(archive, destination)
        return destination

    def test_regular_file_and_internal_framework_symlink(self):
        out = self.extract([("Libbox/Versions/A/Libbox", "file", "binary"),
                            ("Libbox/Versions/Current", "symlink", "A"),
                            ("Libbox/Libbox", "symlink", "Versions/Current/Libbox")])
        self.assertEqual((out / "Libbox/Libbox").read_text(), "binary")

    def test_rejects_traversal_absolute_and_windows_paths(self):
        for name in ("../escape", "/absolute", "a/../../escape", "C:/escape", "a\\escape"):
            with self.subTest(name=name), self.assertRaises(BuildError):
                self.extract([(name, "file", "payload")])

    def test_rejects_escaping_links_link_writes_and_hardlinks(self):
        for entries in ([('a', 'symlink', '../escape')],
                        [('a', 'symlink', '/tmp/escape')],
                        [('a', 'symlink', 'b'), ('a/file', 'file', 'payload')],
                        [('a', 'file', 'payload'), ('b', 'hardlink', 'a')],
                        [('a', 'symlink', 'b'), ('b', 'symlink', 'a')]):
            with self.subTest(entries=entries), self.assertRaises(BuildError):
                self.extract(entries)


class InstallerTests(unittest.TestCase):
    def release_fixture(self, root, links=False, legal_file=False):
        payload = root / "payload"
        payload.mkdir()
        legal = payload / "Legal"
        if legal_file:
            legal.write_text("dependency notice\n")
        else:
            legal.mkdir()
            (legal / "LICENSE").write_text("dependency notice\n")
            if links:
                for name in ("repeat", "again"):
                    (legal / name).symlink_to(".", target_is_directory=True)
        (payload / "LICENSE.sing-box").write_text("upstream notice\n")
        artifact = payload / ARTIFACTS["android"]
        artifact.parent.mkdir(parents=True)
        with zipfile.ZipFile(artifact, "w") as aar:
            aar.writestr("AndroidManifest.xml", "manifest")
            aar.writestr("classes.jar", "classes")
        metadata = {"license_sha256": sha256(payload / "LICENSE.sing-box"),
                    "legal_sha256": artifact_hashes(legal), "artifact_sha256": artifact_hashes(artifact)}
        (payload / "native-build.json").write_text(json.dumps(metadata))
        pins = load_pins()
        source, cache = root / "release", root / "cache"
        source.mkdir()
        cache.mkdir()
        name = archive_name(pins, "android")
        with tarfile.open(source / name, "w:gz") as archive:
            for entry in ("Legal", "LICENSE.sing-box", "native-build.json", ARTIFACTS["android"]):
                archive.add(payload / entry, arcname=entry)
        return pins, source, {name: sha256(source / name)}, cache

    def test_regular_legal_notices_install(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            pins, source, sums, cache = self.release_fixture(root)
            plugin = root / "plugin"
            with mock.patch.object(install_libbox, "ROOT", plugin), mock.patch.object(install_libbox, "validate_metadata"):
                install_libbox.install_one("android", pins, source, sums, cache, False, "test-manifest")
            self.assertTrue((plugin / ARTIFACTS["android"]).is_file())
            self.assertEqual((plugin / ".build/legal/android/LICENSE").read_text(), "dependency notice\n")

    def test_legal_destination_cannot_escape_through_symlinked_parents(self):
        for name, victim_path in [(".build", "legal/android"), (".build/legal", "android"),
                                  (".build/legal/android", ".")]:
            with self.subTest(path=name), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                pins, source, sums, cache = self.release_fixture(root)
                plugin, outside = root / "plugin", root / "outside"
                previous = plugin / ARTIFACTS["android"]
                previous.parent.mkdir(parents=True)
                previous.write_bytes(b"previous-core")
                victim = outside / victim_path
                victim.mkdir(parents=True)
                (victim / "keep").write_bytes(b"external-data")
                link = plugin / name
                link.parent.mkdir(parents=True, exist_ok=True)
                link.symlink_to(outside, target_is_directory=True)
                with mock.patch.object(install_libbox, "ROOT", plugin), mock.patch.object(install_libbox, "validate_metadata"):
                    with self.assertRaisesRegex(BuildError, "inside project .build"):
                        install_libbox.install_one("android", pins, source, sums, cache, True, "test-manifest")
                self.assertEqual((victim / "keep").read_bytes(), b"external-data")
                self.assertEqual(previous.read_bytes(), b"previous-core")
                self.assertFalse((cache / "installed/android.json").exists())

    def test_file_legal_payload_is_rejected_before_replacing_core(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            pins, source, sums, cache = self.release_fixture(root, legal_file=True)
            plugin = root / "plugin"
            previous = plugin / ARTIFACTS["android"]
            previous.parent.mkdir(parents=True)
            previous.write_bytes(b"previous-core")
            with mock.patch.object(install_libbox, "ROOT", plugin), mock.patch.object(install_libbox, "validate_metadata"):
                with self.assertRaisesRegex(BuildError, "legal.*directory"):
                    install_libbox.install_one("android", pins, source, sums, cache, True, "test-manifest")
            self.assertEqual(previous.read_bytes(), b"previous-core")
            self.assertFalse((cache / "installed/android.json").exists())

    def test_invalid_legal_destination_preserves_previous_core(self):
        for name in (".build/legal", ".build/legal/android"):
            with self.subTest(path=name), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                pins, source, sums, cache = self.release_fixture(root)
                plugin = root / "plugin"
                previous = plugin / ARTIFACTS["android"]
                previous.parent.mkdir(parents=True)
                previous.write_bytes(b"previous-core")
                invalid = plugin / name
                invalid.parent.mkdir(parents=True)
                invalid.write_bytes(b"keep")
                with mock.patch.object(install_libbox, "ROOT", plugin), mock.patch.object(install_libbox, "validate_metadata"):
                    with self.assertRaises((BuildError, OSError)):
                        install_libbox.install_one("android", pins, source, sums, cache, True, "test-manifest")
                self.assertEqual(previous.read_bytes(), b"previous-core")
                self.assertEqual(invalid.read_bytes(), b"keep")

    def test_legal_directory_links_are_rejected_before_replacing_core(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            pins, source, sums, cache = self.release_fixture(root, links=True)
            plugin = root / "plugin"
            previous = plugin / ARTIFACTS["android"]
            previous.parent.mkdir(parents=True)
            previous.write_bytes(b"previous-core")
            # Guard the old copying path so a regression cannot expand the links.
            with mock.patch.object(install_libbox, "ROOT", plugin), mock.patch.object(install_libbox, "validate_metadata"), mock.patch("install_libbox.shutil.copytree") as copying:
                with self.assertRaisesRegex(BuildError, "legal.*symlink"):
                    install_libbox.install_one("android", pins, source, sums, cache, True, "test-manifest")
                copying.assert_not_called()
            self.assertEqual(previous.read_bytes(), b"previous-core")
            self.assertFalse((cache / "installed/android.json").exists())


class ToolchainTests(unittest.TestCase):
    def test_environment_always_selects_fork_and_project_caches(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fork = root / "fork"
            inherited = {"GOROOT": "/stock-go", "GOTOOLCHAIN": "auto", "GOFLAGS": "-overlay=bad.json",
                         "GOENV": "malicious", "GOWORK": "workspace", "GOOS": "windows",
                         "GOPROXY": "https://untrusted.invalid", "CGO_CFLAGS": "unsafe", "PATH": "/usr/bin"}
            env = go_environment(fork, root / "cache", inherited)
            self.assertEqual(env["GOROOT"], str(fork))
            self.assertEqual(env["GOTOOLCHAIN"], "local")
            self.assertEqual(env["GOENV"], "off")
            self.assertEqual(env["GOWORK"], "off")
            self.assertEqual(env["PATH"].split(os.pathsep)[0], str(fork / "bin"))
            self.assertNotIn("GOOS", env)
            self.assertNotIn("CGO_CFLAGS", env)
            self.assertEqual(env["GOSUMDB"], "sum.golang.org")
            self.assertTrue(Path(env["GOMODCACHE"]).is_relative_to(root / "cache"))
            self.assertTrue(Path(env["TEST_TELEMETRY_DIR"]).is_relative_to(root / "cache"))
            self.assertEqual((Path(env["TEST_TELEMETRY_DIR"]) / "mode").read_text(), "off\n")

    def test_private_module_rules_survive_pinned_toolchain_environment(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            private = {
                "GOPRIVATE": "git.example.com/team/*,github.com/private-team/*",
                "GONOPROXY": "git.example.com/team/*",
                "GONOSUMDB": "git.example.com/team/*,github.com/private-team/*",
                "GOPROXY": "https://modules.example.com,direct",
                "GOROOT": "/wrong-toolchain", "GOTOOLCHAIN": "auto",
                "GOSUMDB": "off", "GOFLAGS": "-overlay=unexpected.json",
            }
            env = go_environment(root / "verified-fork", root / "cache", private)
            for name in ("GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOPROXY"):
                self.assertEqual(env[name], private[name])
            self.assertEqual(env["GOROOT"], str(root / "verified-fork"))
            self.assertEqual(env["GOTOOLCHAIN"], "local")
            self.assertEqual(env["GOSUMDB"], "sum.golang.org")
            self.assertNotIn("overlay", env["GOFLAGS"])

    def test_goprivate_uses_normal_go_defaults_without_forced_scope_overrides(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            env = go_environment(root / "fork", root / "cache", {
                "GOPRIVATE": "git.example.com/team/*",
            })
            self.assertEqual(env["GOPRIVATE"], "git.example.com/team/*")
            self.assertNotIn("GONOPROXY", env)
            self.assertNotIn("GONOSUMDB", env)
            self.assertEqual(env["GOPROXY"], "https://proxy.golang.org,direct")
            explicit = go_environment(root / "fork", root / "cache", {
                "GOPRIVATE": "git.example.com/team/*", "GONOPROXY": "none",
            })
            self.assertEqual(explicit["GONOPROXY"], "none")

    def test_resolved_tailscale_upgrade_is_verified_and_recorded(self):
        pins = load_pins()
        for compatible in (True, False):
            with self.subTest(compatible=compatible), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                (root / "go.toolchain.rev").write_text(pins["tailscale_go_revision"] if compatible else "1" * 40)
                (root / "go.toolchain.next.rev").write_text("2" * 40)
                selected = {"Path": "github.com/sagernet/tailscale", "Version": "v1.99.0",
                            "Dir": str(root), "Sum": "h1:resolved", "GoModSum": "h1:resolved-mod"}
                with mock.patch("build_libbox.network_run", return_value=json.dumps(selected)) as download:
                    if not compatible:
                        with self.assertRaisesRegex(BuildError, "runtime requirement"):
                            resolved_tailscale_dependency(Path("verified-go"), {}, root, pins)
                    else:
                        identity = resolved_tailscale_dependency(Path("verified-go"), {}, root, pins)
                        self.assertEqual(identity["version"], selected["Version"])
                        self.assertEqual(identity["sum"], selected["Sum"])
                        self.assertEqual(identity["go_mod_sum"], selected["GoModSum"])
                        self.assertEqual(identity["toolchain_revision"], pins["tailscale_go_revision"])
                    self.assertIn("github.com/sagernet/tailscale", download.call_args.args[0])

    def test_runtime_toolchain_contract_rejects_a_different_fork(self):
        actual = "63ae404c8203317fd3c82d972e5dc8f0fcb425cb"
        verify_dependency_toolchain(actual, actual + "\n", "1" * 40)
        verify_dependency_toolchain(actual, "1" * 40, actual)
        with self.assertRaisesRegex(BuildError, "runtime requirement"):
            verify_dependency_toolchain("7ef7b403648ac3a9481a6e88e8b894eff50b0167", actual, actual)

    def test_corrupt_toolchain_cache_is_never_executed(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bootstrap = root / "bootstrap"
            bootstrap.mkdir()
            cache = root / "cache"
            (cache / "archives").mkdir(parents=True)
            pins = load_pins()
            archive = cache / "archives" / f"tailscale-go-{pins['tailscale_go_revision']}-darwin-arm64.tar.gz"
            archive.write_bytes(b"not a verified release")
            with mock.patch("build_libbox.host_key", return_value="darwin-arm64"), mock.patch("build_libbox.run") as run:
                with self.assertRaisesRegex(BuildError, "Cached Tailscale Go archive"):
                    bootstrap_go(pins, bootstrap, cache)
                run.assert_not_called()

    def test_bootstrap_cache_paths_cannot_escape_build(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            self.assertEqual(build_directory(root / ".build/cache", root), root.resolve() / ".build/cache")
            with self.assertRaises(BuildError):
                build_directory(root / "elsewhere", root)
            (root / ".build").symlink_to(root / "outside")
            with self.assertRaises(BuildError):
                build_directory(root / ".build/cache", root)


if __name__ == "__main__":
    unittest.main()
