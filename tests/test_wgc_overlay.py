"""WGC build registration, integrity, and restoration tests without a Go build."""
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
from native_common import BuildError, ROOT, load_pins, pin_identity, sha256, validate_metadata, REQUIRED_TAGS
from wgc_overlay import load_extensions, source_digest, wgc_overlay


class OverlayTests(unittest.TestCase):
    def fixture(self, root):
        source = root / "upstream"
        registry = source / "include/registry.go"
        builder = source / "cmd/internal/build_libbox/main.go"
        registry.parent.mkdir(parents=True)
        builder.parent.mkdir(parents=True)
        registry.write_text("package include\nfunc EndpointRegistry() {\n\tregisterTailscaleEndpoint(registry)\n}\n")
        builder.write_text('package main\nfunc init() { sharedTags = append(sharedTags, "with_gvisor") }\n')
        (source / "go.mod").write_text("module github.com/sagernet/sing-box\n")
        (source / "go.sum").write_text("original sums\n")
        (root / "native/wgc").mkdir(parents=True)
        (root / "native/wgc/go.mod").write_text("module github.com/idy/sing-box-flutter/native/wgc\n")
        (root / "native-core.json").write_text(json.dumps({"wgc_version": "v1.2.3"}))
        (root / "native/wgc/go.sum").write_text("github.com/idy/wgc v1.2.3 h1:locked-module\ngithub.com/idy/wgc v1.2.3/go.mod h1:locked-mod\n")
        (root / "native/wgc/endpoint.go").write_text("package core\n")
        (root / "tool").mkdir()
        (root / "tool/wgc_overlay.py").write_bytes((ROOT / "tool/wgc_overlay.py").read_bytes())
        return source, registry, builder

    def test_registration_and_go_inputs_are_restored_after_a_failed_build(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, registry, builder = self.fixture(root)
            originals = {p: p.read_bytes() for p in [registry, builder, source / "go.mod", source / "go.sum"]}
            def fake_run(args, **kwargs):
                if args[1:3] == ["mod", "tidy"]:
                    self.assertEqual(kwargs["env"]["GONOSUMDB"], "github.com/idy/wgc")
                    self.assertIn("github.com/idy/wgc v1.2.3 h1:locked-module", (source / "go.sum").read_text())
                    self.assertIn("github.com/idy/wgc v1.2.3/go.mod h1:locked-mod", (source / "go.sum").read_text())
                    (source / "go.sum").write_text("new dependency checksums\n")
                return ""
            with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                with self.assertRaisesRegex(RuntimeError, "compilation failure"):
                    with wgc_overlay(source, Path("go"), {"GONOSUMDB": "github.com/idy/wgc"}, fake_run, [], root) as provenance:
                        self.assertIn("registerWGCEndpoint(registry)", registry.read_text())
                        self.assertIn('"with_wgc"', builder.read_text())
                        self.assertIn("wgc.RegisterEndpoint(registry)", (source / "include/wgc_flutter.go").read_text())
                        self.assertEqual(provenance["source_sha256"], source_digest(root / "native/wgc"))
                        self.assertEqual(provenance["extensions"], [])
                        raise RuntimeError("compilation failure")
            for path, original in originals.items():
                self.assertEqual(path.read_bytes(), original)
            self.assertFalse((source / "include/wgc_flutter.go").exists())

    def test_conflicting_wgc_checksums_fail_before_resolution_and_restore_inputs(self):
        for version in ("v1.2.3", "v1.2.3/go.mod"):
            with self.subTest(version=version), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                source, registry, builder = self.fixture(root)
                original = f"github.com/idy/wgc {version} h1:untrusted\n"
                (source / "go.sum").write_text(original)
                runner = mock.Mock(return_value="")
                with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                    with self.assertRaisesRegex(BuildError, "checksum conflicts"):
                        with wgc_overlay(source, Path("go"), {"GONOSUMDB": "github.com/idy/wgc"}, runner, [], root):
                            self.fail("Untrusted WGC checksum reached compilation")
                self.assertFalse(any(call.args[0][1:3] == ["mod", "tidy"] for call in runner.call_args_list))
                self.assertEqual((source / "go.sum").read_text(), original)
                self.assertNotIn("registerWGCEndpoint", registry.read_text())

    def test_missing_wgc_lock_fails_before_resolution(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, registry, _ = self.fixture(root)
            (root / "native/wgc/go.sum").write_text("github.com/idy/wgc v1.2.3 h1:locked-module\n")
            runner = mock.Mock(return_value="")
            with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                with self.assertRaisesRegex(BuildError, "checksums are required"):
                    with wgc_overlay(source, Path("go"), {}, runner, [], root):
                        self.fail("Missing WGC lock reached resolution")
            self.assertFalse(any(call.args[0][1:3] == ["mod", "tidy"] for call in runner.call_args_list))

    def test_unreviewed_upstream_registry_is_never_modified(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, registry, builder = self.fixture(root)
            original = registry.read_bytes()
            runner = mock.Mock()
            with self.assertRaisesRegex(BuildError, "reviewed WGC"):
                with wgc_overlay(source, Path("go"), {}, runner, [], root):
                    pass
            runner.assert_not_called()
            self.assertEqual(registry.read_bytes(), original)

    def test_source_identity_tracks_module_files_but_ignores_vcs_metadata(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "go.mod").write_text("module example.com/transport\n")
            code = root / "register.go"
            code.write_text("package register\n")
            before = source_digest(root)
            (root / ".git").mkdir()
            (root / ".git/HEAD").write_text("ref: refs/heads/main\n")
            self.assertEqual(source_digest(root), before)
            (root / "register_test.go").write_text("package register\n// tests included conservatively\n")
            self.assertNotEqual(source_digest(root), before)
            before = source_digest(root)
            code.write_text("package register\nfunc init() {}\n")
            self.assertNotEqual(source_digest(root), before)

    def test_binary_contents_cannot_hide_a_separate_source_file(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "go.mod").write_text("module example.com/transport\n")
            (root / "transport.go").write_text("package transport\n")
            registration = b"package transport\nfunc init() {}\n"
            (root / "payload.bin").write_bytes(b"X")
            (root / "register.go").write_bytes(registration)
            before = source_digest(root)
            (root / "payload.bin").write_bytes(b"X\0register.go\0" + registration)
            (root / "register.go").unlink()
            self.assertNotEqual(source_digest(root), before)

    def test_source_identity_includes_assembly_and_embedded_assets(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "go.mod").write_text("module example.com/transport\n")
            (root / "register.go").write_text('package register\nimport "embed"\n//go:embed all:assets\nvar data embed.FS\n')
            (root / "assets").mkdir()
            assembly = root / "transport_amd64.s"
            asset = root / "assets/table.bin"
            hidden_asset = root / "assets/.dictionary"
            assembly.write_text("// first assembly implementation\n")
            asset.write_bytes(b"first payload")
            hidden_asset.write_bytes(b"first dictionary")
            for path, replacement in [(assembly, b"// changed implementation\n"),
                                      (asset, b"changed payload"),
                                      (hidden_asset, b"changed dictionary")]:
                before = source_digest(root)
                path.write_bytes(replacement)
                self.assertNotEqual(source_digest(root), before, str(path))


class ExtensionTests(unittest.TestCase):
    def test_compiled_local_extension_records_source_identity(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            extension = root / "transport"
            extension.mkdir()
            (extension / "go.mod").write_text("module example.com/transport\n")
            (extension / "register.go").write_text("package transport\n")
            config = root / "wgc.core.json"
            config.write_text(json.dumps({"extensions": [{"package": "example.com/transport/register", "module": "example.com/transport", "path": "transport"}]}))
            entries = load_extensions(config)
            self.assertEqual(entries[0]["path"], str(extension.resolve()))
            self.assertEqual(entries[0]["source_sha256"], source_digest(extension))
            self.assertEqual(entries[0]["version"], "v0.0.0")

    def test_local_extension_versions_match_the_module_major(self):
        for module, explicit, expected in [
            ("example.com/transport/v2", None, "v2.0.0"),
            ("example.com/transport/v10", None, "v10.0.0"),
            ("gopkg.in/transport.v3", None, "v3.0.0"),
            ("example.com/transport/v2", "v2.7.3", "v2.7.3"),
        ]:
            with self.subTest(module=module, explicit=explicit), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                extension = root / "transport"
                extension.mkdir()
                (extension / "go.mod").write_text(f"module {module}\n")
                entry = {"package": module + "/register", "module": module, "path": "transport"}
                if explicit is not None:
                    entry["version"] = explicit
                config = root / "wgc.core.json"
                config.write_text(json.dumps({"extensions": [entry]}))
                self.assertEqual(load_extensions(config)[0]["version"], expected)

    def test_arbitrary_go_imports_and_reserved_modules_are_rejected(self):
        for package, module in [("bad\"\nimport os", "example.com/bad"),
                                ("example.com/a/../b", "example.com/a"),
                                ("github.com/idy/wgc", "github.com/idy/wgc"),
                                ("example.com/other", "example.com/module")]:
            with self.subTest(package=package), tempfile.TemporaryDirectory() as temp:
                config = Path(temp) / "wgc.core.json"
                config.write_text(json.dumps({"extensions": [{"package": package, "module": module}]}))
                with self.assertRaises(BuildError):
                    load_extensions(config)


if __name__ == "__main__":
    unittest.main()
