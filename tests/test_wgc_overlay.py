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
                    (source / "go.sum").write_text("new dependency checksums\n")
                return ""
            with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                with self.assertRaisesRegex(RuntimeError, "compilation failure"):
                    with wgc_overlay(source, Path("go"), {}, fake_run, [], root) as provenance:
                        self.assertIn("registerWGCEndpoint(registry)", registry.read_text())
                        self.assertIn('"with_wgc"', builder.read_text())
                        self.assertIn("wgc.RegisterEndpoint(registry)", (source / "include/wgc_flutter.go").read_text())
                        self.assertEqual(provenance["source_sha256"], source_digest(root / "native/wgc"))
                        self.assertEqual(provenance["extensions"], [])
                        raise RuntimeError("compilation failure")
            for path, original in originals.items():
                self.assertEqual(path.read_bytes(), original)
            self.assertFalse((source / "include/wgc_flutter.go").exists())

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

    def test_source_identity_tracks_code_but_ignores_tests(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "go.mod").write_text("module example.com/transport\n")
            code = root / "register.go"
            code.write_text("package register\n")
            before = source_digest(root)
            (root / "register_test.go").write_text("package register\n// tests only\n")
            self.assertEqual(source_digest(root), before)
            code.write_text("package register\nfunc init() {}\n")
            self.assertNotEqual(source_digest(root), before)


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
