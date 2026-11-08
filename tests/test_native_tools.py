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

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
from native_common import (BuildError, ROOT, REQUIRED_TAGS, build_directory, load_pins,
                           parse_manifest, pin_identity, safe_extract, sha256, validate_metadata)
from build_libbox import bootstrap_go, go_environment, mobile_profile_bytes


class PinTests(unittest.TestCase):
    def test_expected_user_pins(self):
        self.assertEqual(pin_identity(load_pins()), {
            "sing_box_version": "1.14.4",
            "sing_box_revision": "c005f35300ce76427dc285481ec5695f5b179cd2",
            "tailscale_go_revision": "7ef7b403648ac3a9481a6e88e8b894eff50b0167",
            "gomobile_version": "v0.1.12",
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
                    "mobile_profile": {"profile_sha256": sha256(ROOT / "tool/mobile-profile.json"),
                                       "upstream_builder_sha256": profile["upstream_builder_sha256"],
                                       "removed_tags": profile["drop_tags"]}}
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
