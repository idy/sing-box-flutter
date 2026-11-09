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
from wgc_overlay import MODULE, load_extensions, source_digest, wgc_overlay
from source_inputs import ADAPTER_INPUTS

EXTENSION_LOCK = {"sum": "h1:" + "A" * 43 + "=", "go_mod_sum": "h1:" + "B" * 42 + "A="}


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
        for name in ADAPTER_INPUTS - {"go.mod", "go.sum"}:
            (root / "native/wgc" / name).write_text("package core\n")
        (root / "tool").mkdir()
        (root / "tool/wgc_overlay.py").write_bytes((ROOT / "tool/wgc_overlay.py").read_bytes())
        return source, registry, builder

    def remote_downloads(self, root, extensions, dependency_sums=""):
        downloads = {}
        for index, entry in enumerate(extensions):
            module_root = root / f"remote-{index}"
            module_root.mkdir()
            (module_root / "go.sum").write_text(dependency_sums)
            downloads[f"{entry['module']}@{entry['version']}"] = json.dumps({
                "Dir": str(module_root), "Sum": entry["sum"], "GoModSum": entry["go_mod_sum"],
            })
        return downloads

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

    def test_exact_extension_versions_reach_compilation_and_restore_inputs(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, registry, builder = self.fixture(root)
            originals = {p: p.read_bytes() for p in [registry, builder, source / "go.mod", source / "go.sum"]}
            extensions = [
                {"package": "example.com/a/register", "module": "example.com/a", "version": "v1.2.3"},
                {"package": "example.com/b/register", "module": "example.com/b", "version": "v1.4.0"},
            ]
            for entry in extensions:
                entry.update(EXTENSION_LOCK)
            downloads = self.remote_downloads(root, extensions)
            selected = {"example.com/a": "v1.2.3", "example.com/b": "v1.4.0"}
            env = {"GOFLAGS": "-mod=readonly", "GOWORK": "off", "GONOSUMDB": "example.com/*"}
            def fake_run(args, **kwargs):
                if args[1:3] == ["mod", "tidy"]:
                    for entry in extensions:
                        self.assertIn(f"{entry['module']} {entry['version']} {entry['sum']}\n", (source / "go.sum").read_text())
                        self.assertIn(f"{entry['module']} {entry['version']}/go.mod {entry['go_mod_sum']}\n", (source / "go.sum").read_text())
                    (source / "go.mod").write_text("module github.com/sagernet/sing-box\n// resolved dependencies\n")
                    (source / "go.sum").write_text("resolved checksums\n")
                if args[1:3] == ["mod", "download"]:
                    return downloads[args[-1]]
                if args[1:3] == ["list", "-m"]:
                    return json.dumps({"Version": selected[args[-1]], "Sum": EXTENSION_LOCK["sum"], "GoModSum": EXTENSION_LOCK["go_mod_sum"]})
                return ""
            runner = mock.Mock(side_effect=fake_run)
            with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                with wgc_overlay(source, Path("go"), env, runner, extensions, root) as provenance:
                    self.assertEqual(provenance["extensions"], extensions)
                    self.assertEqual(provenance["go_sum_sha256"], sha256(source / "go.sum"))
            self.assertEqual(runner.call_args_list[-4:], [
                mock.call([Path("go"), "mod", "tidy"], cwd=source,
                          env=dict(env, GOFLAGS="-trimpath -buildvcs=false")),
                mock.call([Path("go"), "list", "-m", "-json", "example.com/a"],
                          cwd=source, env=env, capture=True),
                mock.call([Path("go"), "list", "-m", "-json", "example.com/b"],
                          cwd=source, env=env, capture=True),
                mock.call([Path("go"), "mod", "verify"], cwd=source, env=env),
            ])
            for path, original in originals.items():
                self.assertEqual(path.read_bytes(), original)
            self.assertFalse((source / "include/wgc_flutter.go").exists())

    def test_mvs_extension_upgrade_fails_before_compilation_and_restores_inputs(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, registry, builder = self.fixture(root)
            originals = {p: p.read_bytes() for p in [registry, builder, source / "go.mod", source / "go.sum"]}
            extensions = [
                {"package": "example.com/a", "module": "example.com/a", "version": "v1.0.0"},
                {"package": "example.com/b", "module": "example.com/b", "version": "v1.0.0"},
            ]
            for entry in extensions:
                entry.update(EXTENSION_LOCK)
            downloads = self.remote_downloads(root, extensions)
            selected = {"example.com/a": "v1.0.0", "example.com/b": "v1.1.0"}
            env = {"GOFLAGS": "-mod=readonly", "GOWORK": "off"}
            def fake_run(args, **kwargs):
                if args[1:3] == ["mod", "tidy"]:
                    # A requires B v1.1.0, so MVS upgrades the configured B v1.0.0.
                    (source / "go.mod").write_text("module github.com/sagernet/sing-box\nrequire example.com/b v1.1.0\n")
                    (source / "go.sum").write_text("upgraded dependency checksums\n")
                if args[1:3] == ["mod", "download"]:
                    return downloads[args[-1]]
                if args[1:3] == ["list", "-m"]:
                    return json.dumps({"Version": selected[args[-1]], "Sum": EXTENSION_LOCK["sum"], "GoModSum": EXTENSION_LOCK["go_mod_sum"]})
                return ""
            runner = mock.Mock(side_effect=fake_run)
            with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                with self.assertRaisesRegex(BuildError, r"example\.com/b resolved to v1\.1\.0, expected v1\.0\.0"):
                    with wgc_overlay(source, Path("go"), env, runner, extensions, root):
                        self.fail("MVS-upgraded extension reached compilation with stale provenance")
            self.assertEqual(runner.call_args_list[-2:], [
                mock.call([Path("go"), "list", "-m", "-json", "example.com/a"],
                          cwd=source, env=env, capture=True),
                mock.call([Path("go"), "list", "-m", "-json", "example.com/b"],
                          cwd=source, env=env, capture=True),
            ])
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

    def test_remote_checksum_conflicts_fail_before_resolution(self):
        for suffix in ("", "/go.mod"):
            with self.subTest(suffix=suffix), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                source, registry, _ = self.fixture(root)
                original = f"example.com/transport v1.2.3{suffix} h1:untrusted\n"
                (source / "go.sum").write_text(original)
                entry = {"package": "example.com/transport", "module": "example.com/transport", "version": "v1.2.3", **EXTENSION_LOCK}
                runner = mock.Mock(return_value="")
                with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                    with self.assertRaisesRegex(BuildError, "checksum conflicts"):
                        with wgc_overlay(source, Path("go"), {}, runner, [entry], root):
                            self.fail("Conflicting remote checksum reached compilation")
                self.assertFalse(any(call.args[0][1:3] == ["mod", "tidy"] for call in runner.call_args_list))
                self.assertEqual((source / "go.sum").read_text(), original)

    def test_local_modules_compile_from_verified_snapshots(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, registry, _ = self.fixture(root)
            local = root / "local"
            local.mkdir()
            (local / "go.mod").write_text("module example.com/transport\n")
            (local / "register.go").write_text("package transport\n// original\n")
            config = root / "extensions.json"
            config.write_text(json.dumps({"extensions": [{"package": "example.com/transport", "module": "example.com/transport", "path": "local"}]}))
            extensions = load_extensions(config)
            original_adapter = (root / "native/wgc/endpoint.go").read_bytes()
            replacements = {}

            def runner(args, **kwargs):
                if args[1:3] == ["mod", "edit"]:
                    for flag in args[3:]:
                        if str(flag).startswith("-replace="):
                            module, path = str(flag)[len("-replace="):].split("=", 1)
                            replacements[module] = Path(path)
                if args[1:3] == ["mod", "tidy"]:
                    (local / "register.go").write_text("package transport\n// edited during build\n")
                    (root / "native/wgc/endpoint.go").write_text("package core\n// edited during build\n")
                return json.dumps({"Version": "v0.0.0", "Dir": str(replacements["example.com/transport"])}) if args[1:3] == ["list", "-m"] else ""

            with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                with wgc_overlay(source, Path("go"), {}, runner, extensions, root) as provenance:
                    snapshot = replacements["example.com/transport"]
                    self.assertNotEqual(snapshot.resolve(), local.resolve())
                    self.assertIn("// original", (snapshot / "register.go").read_text())
                    self.assertEqual(source_digest(snapshot), extensions[0]["source_sha256"])
                    self.assertEqual(provenance["extensions"][0]["source_sha256"], source_digest(snapshot))
                    adapter_snapshot = replacements[MODULE]
                    self.assertNotEqual(adapter_snapshot.resolve(), (root / "native/wgc").resolve())
                    self.assertEqual((adapter_snapshot / "endpoint.go").read_bytes(), original_adapter)
                    self.assertEqual(source_digest(adapter_snapshot), provenance["source_sha256"])
            self.assertFalse(snapshot.exists())
            self.assertFalse(adapter_snapshot.exists())
            self.assertNotIn("path", provenance["extensions"][0])

    def test_multiple_packages_in_one_module_reuse_the_same_snapshot(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, registry, _ = self.fixture(root)
            local = root / "local"
            local.mkdir()
            (local / "go.mod").write_text("module example.com/transport\n")
            for name in ("a", "b"):
                (local / name).mkdir()
                (local / name / "register.go").write_text(f"package {name}\n")
            config = root / "extensions.json"
            config.write_text(json.dumps({"extensions": [
                {"package": f"example.com/transport/{name}", "module": "example.com/transport", "path": "local"}
                for name in ("a", "b")
            ]}))
            entries = load_extensions(config)
            replacements = []
            def runner(args, **kwargs):
                if args[1:3] == ["mod", "edit"]:
                    for flag in args[3:]:
                        if str(flag).startswith("-replace=example.com/transport="):
                            replacements.append(Path(str(flag).split("=", 2)[2]))
                if args[1:3] == ["list", "-m"]:
                    return json.dumps({"Version": "v0.0.0", "Dir": str(replacements[-1])})
                return ""
            with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                with wgc_overlay(source, Path("go"), {}, runner, entries, root) as provenance:
                    self.assertEqual(len(replacements), 2)
                    self.assertEqual(replacements[0], replacements[1])
                    self.assertNotEqual(replacements[0].resolve(), local.resolve())
                    self.assertEqual(source_digest(replacements[0]), entries[0]["source_sha256"])
                    self.assertEqual(len(provenance["extensions"]), 2)
                    for entry in provenance["extensions"]:
                        self.assertEqual(entry["source_sha256"], entries[0]["source_sha256"])
            self.assertFalse(replacements[0].exists())

    def test_local_changes_before_snapshot_are_rejected(self):
        for change in ("contents", "symlink"):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                source, registry, _ = self.fixture(root)
                local = root / "local"
                local.mkdir()
                (local / "go.mod").write_text("module example.com/transport\n")
                code = local / "register.go"
                code.write_text("package transport\n")
                config = root / "extensions.json"
                config.write_text(json.dumps({"extensions": [{"package": "example.com/transport", "module": "example.com/transport", "path": "local"}]}))
                entries = load_extensions(config)
                if change == "contents":
                    code.write_text("package transport\n// changed\n")
                else:
                    (root / "outside").write_text("outside data")
                    (local / "link").symlink_to(root / "outside")
                runner = mock.Mock(return_value="")
                with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                    with self.assertRaises(BuildError):
                        with wgc_overlay(source, Path("go"), {}, runner, entries, root):
                            self.fail("Changed local source reached compilation")
                runner.assert_not_called()
                self.assertFalse(list((root / ".build").glob("wgc-inputs-*")))

    def test_authenticated_extension_sums_lock_transitive_dependencies(self):
        dependency = (f"example.com/private/dep v1.0.0 {EXTENSION_LOCK['sum']}\n"
                      f"example.com/private/dep v1.0.0/go.mod {EXTENSION_LOCK['go_mod_sum']}\n")
        for locked in (True, False):
            with self.subTest(locked=locked), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                source, registry, _ = self.fixture(root)
                entry = {"package": "example.com/transport", "module": "example.com/transport", "version": "v1.2.3", **EXTENSION_LOCK}
                downloads = self.remote_downloads(root, [entry], dependency if locked else "")
                original = (source / "go.sum").read_bytes()

                def runner(args, **kwargs):
                    if args[1:3] == ["mod", "download"]:
                        return downloads[args[-1]]
                    if args[1:3] == ["mod", "tidy"]:
                        if locked:
                            self.assertIn(dependency, (source / "go.sum").read_text())
                        else:
                            (source / "go.sum").write_text((source / "go.sum").read_text() + dependency)
                    if args[1:3] == ["list", "-m"]:
                        return json.dumps({"Version": entry["version"], "Sum": entry["sum"], "GoModSum": entry["go_mod_sum"]})
                    return ""

                with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                    if locked:
                        with wgc_overlay(source, Path("go"), {"GONOSUMDB": "example.com/*"}, runner, [entry], root):
                            pass
                    else:
                        with self.assertRaisesRegex(BuildError, "Unpinned extension dependency"):
                            with wgc_overlay(source, Path("go"), {"GONOSUMDB": "example.com/*"}, runner, [entry], root):
                                self.fail("Unpinned private dependency reached compilation")
                self.assertEqual((source / "go.sum").read_bytes(), original)

    def test_conflicting_authenticated_dependency_lock_is_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, registry, _ = self.fixture(root)
            original = "example.com/private/dep v1.0.0 h1:untrusted\n"
            (source / "go.sum").write_text(original)
            entry = {"package": "example.com/transport", "module": "example.com/transport", "version": "v1.2.3", **EXTENSION_LOCK}
            downloads = self.remote_downloads(root, [entry], f"example.com/private/dep v1.0.0 {EXTENSION_LOCK['sum']}\n")
            def run(args, **kwargs):
                return downloads[args[-1]] if args[1:3] == ["mod", "download"] else ""
            runner = mock.Mock(side_effect=run)
            with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                with self.assertRaisesRegex(BuildError, "checksum conflicts"):
                    with wgc_overlay(source, Path("go"), {}, runner, [entry], root):
                        self.fail("Conflicting dependency lock reached compilation")
            self.assertFalse(any(call.args[0][1:3] == ["mod", "tidy"] for call in runner.call_args_list))
            self.assertEqual((source / "go.sum").read_text(), original)

    def test_remote_replacement_cannot_bypass_pinned_source(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source, registry, _ = self.fixture(root)
            entry = {"package": "example.com/transport", "module": "example.com/transport", "version": "v1.2.3", **EXTENSION_LOCK}
            downloads = self.remote_downloads(root, [entry])
            def runner(args, **kwargs):
                if args[1:3] == ["mod", "download"]:
                    return downloads[args[-1]]
                if args[1:3] == ["list", "-m"]:
                    return json.dumps({"Version": entry["version"], "Sum": entry["sum"], "GoModSum": entry["go_mod_sum"], "Replace": {"Path": "example.com/other"}})
                return ""
            with mock.patch("wgc_overlay.REGISTRY_SHA256", sha256(registry)):
                with self.assertRaisesRegex(BuildError, "source differs"):
                    with wgc_overlay(source, Path("go"), {}, runner, [entry], root):
                        self.fail("Replacement source reached compilation with another module's pins")

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
    def test_remote_extensions_require_persisted_checksum_pins(self):
        with tempfile.TemporaryDirectory() as temp:
            config = Path(temp) / "extensions.json"
            base = {"package": "example.com/transport", "module": "example.com/transport", "version": "v1.2.3"}
            for pins in ({}, {"sum": EXTENSION_LOCK["sum"]}, {**EXTENSION_LOCK, "sum": "bad"},
                         {**EXTENSION_LOCK, "go_mod_sum": "bad"}):
                with self.subTest(pins=pins):
                    config.write_text(json.dumps({"extensions": [{**base, **pins}]}))
                    with self.assertRaisesRegex(BuildError, "checksum"):
                        load_extensions(config)
            config.write_text(json.dumps({"extensions": [{**base, **EXTENSION_LOCK}]}))
            loaded = load_extensions(config)[0]
            self.assertEqual(loaded["sum"], EXTENSION_LOCK["sum"])
            self.assertEqual(loaded["go_mod_sum"], EXTENSION_LOCK["go_mod_sum"])

    def test_conflicting_module_checksum_pins_are_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            config = Path(temp) / "extensions.json"
            first = {"package": "example.com/transport/a", "module": "example.com/transport", "version": "v1.2.3", **EXTENSION_LOCK}
            second = {**first, "package": "example.com/transport/b", "sum": EXTENSION_LOCK["go_mod_sum"]}
            config.write_text(json.dumps({"extensions": [first, second]}))
            with self.assertRaises(BuildError):
                load_extensions(config)

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
