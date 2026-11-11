"""Install-record CLI integration; this test process stands in for parent CMake."""
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest


sys.dont_write_bytecode = True
TOOL = Path(__file__).resolve().parents[1] / "tool/windows_install_records.py"
_READER = """
import json
from pathlib import Path
import sys
sys.path.insert(0, str(Path(sys.argv[1]).resolve().parent))
from windows_install_records import read_records
lease = json.loads(sys.argv[2])
records = read_records(lease)
assert read_records(lease) == records, 'reads must seek back to the beginning'
print(json.dumps(records, ensure_ascii=True))
"""


def child_python(*arguments):
    return subprocess.run([sys.executable, "-I", "-B", "-X", "utf8", *map(str, arguments)],
                          capture_output=True, text=True, encoding="utf-8", timeout=30)


def windows_handle_path(handle):
    import ctypes
    from ctypes import wintypes as W
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.GetFinalPathNameByHandleW.argtypes = [W.HANDLE, W.LPWSTR, W.DWORD, W.DWORD]
    kernel.GetFinalPathNameByHandleW.restype = W.DWORD
    buffer = ctypes.create_unicode_buffer(32768)
    size = kernel.GetFinalPathNameByHandleW(handle, buffer, len(buffer), 0)
    if not 0 < size < len(buffer):
        raise ctypes.WinError(ctypes.get_last_error())
    return Path(buffer.value)


class InstallRecordTransport(unittest.TestCase):
    def setUp(self):
        self.leases = []
        self.addCleanup(self.cleanup_leases)

    def cleanup_leases(self):
        for lease in self.leases:
            result = self.command("release", "--lease", json.dumps(lease))
            self.assertEqual(result.returncode, 0, result.stderr)

    def command(self, *arguments):
        return child_python(TOOL, *arguments)

    def create(self, *arguments):
        result = self.command("create", *arguments)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, "")
        lease = json.loads(result.stdout)
        self.assertEqual(lease["parent_pid"], os.getpid())
        self.assertEqual(lease["os"], os.name)
        self.leases.append(lease)
        return lease

    def append(self, lease, content):
        result = self.command("append", "--lease", json.dumps(lease), "--hex", content.hex())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((result.stdout, result.stderr), ("", ""))

    def read(self, lease):
        result = child_python("-c", _READER, TOOL, json.dumps(lease))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, "")
        return json.loads(result.stdout)

    def release(self, lease):
        result = self.command("release", "--lease", json.dumps(lease))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((result.stdout, result.stderr), ("", ""))
        self.leases.remove(lease)

    def portable_path(self, lease):
        return Path(tempfile.gettempdir()) / lease["directory"] / lease["filename"]

    def test_exact_unicode_semicolons_and_large_hex_chunks(self):
        lease = self.create()
        self.assertEqual(self.read(lease), "")
        # Put the first byte of a four-byte codepoint at byte 4095, so the
        # first 8192-character hex chunk necessarily splits that codepoint.
        manifest = "C:/" + "a" * 4092 + "🙂;"
        manifest += "C:/应用;目录/é🙂 helper.exe;D:/行\n次;\r\n" * 2800
        raw = manifest.encode("utf-8")
        self.assertGreater(len(raw), 2 * 65536)
        encoded = raw.hex()
        chunks = [encoded[start:start + 8192] for start in range(0, len(encoded), 8192)]
        split_codepoint = False
        for chunk in chunks:
            try:
                bytes.fromhex(chunk).decode("utf-8")
            except UnicodeDecodeError:
                split_codepoint = True
            result = self.command("append", "--lease", json.dumps(lease), "--hex", chunk)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((result.stdout, result.stderr), ("", ""))
        self.assertTrue(split_codepoint, "exercise UTF-8 codepoints split between commands")
        self.assertEqual(self.read(lease), manifest)
        self.append(lease, b"")
        self.append(lease, ";C:/追加/helper.exe".encode("utf-8"))
        self.assertEqual(self.read(lease), manifest + ";C:/追加/helper.exe")

    def test_invocations_are_distinct_and_release_is_independent(self):
        first, second = self.create(), self.create()
        self.assertNotEqual(first, second)
        if os.name == "nt":
            self.assertNotEqual(first["handle"], second["handle"])
        else:
            self.assertNotEqual(first["token"], second["token"])
            self.assertNotEqual(first["directory"], second["directory"])
        self.append(first, b"first;manifest")
        self.append(second, b"second;manifest")
        self.assertEqual(self.read(first), "first;manifest")
        self.assertEqual(self.read(second), "second;manifest")
        self.release(first)
        self.assertEqual(self.read(second), "second;manifest")

    def test_compatibility_directory_is_ignored_and_release_removes_storage(self):
        with tempfile.TemporaryDirectory() as build_directory:
            lease = self.create("--directory", build_directory)
            self.assertEqual(list(Path(build_directory).iterdir()), [])
            if os.name == "nt":
                self.assertEqual(set(lease), {"os", "parent_pid", "handle"})
                path = windows_handle_path(lease["handle"])
            else:
                path = self.portable_path(lease)
                self.assertEqual(path.parent.parent, Path(tempfile.gettempdir()))
                self.assertEqual(stat.S_IMODE(path.parent.stat().st_mode), 0o700)
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.append(lease, b"preserved;until;release")
            self.release(lease)
            self.assertFalse(path.exists())
            if os.name != "nt":
                self.assertFalse(path.parent.exists())

    def test_other_parent_is_rejected_without_changing_original_records(self):
        lease = self.create()
        self.append(lease, b"original")
        wrong = {**lease, "parent_pid": os.getpid() + 1}
        for operation, extra in (("append", ["--hex", "616c7465726564"]), ("release", [])):
            with self.subTest(operation=operation):
                result = self.command(operation, "--lease", json.dumps(wrong), *extra)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("invoking parent", result.stderr)
                self.assertEqual(result.stdout, "")
        reader = child_python("-c", _READER, TOOL, json.dumps(wrong))
        self.assertNotEqual(reader.returncode, 0)
        self.assertIn("invoking parent", reader.stderr)
        # A grandchild cannot use a genuine descriptor belonging to this test.
        wrapper = """
import subprocess, sys
raise SystemExit(subprocess.run([sys.executable, '-I', '-B', *sys.argv[1:]]).returncode)
"""
        nested = child_python("-c", wrapper, TOOL, "append", "--lease", json.dumps(lease),
                              "--hex", "616c7465726564")
        self.assertNotEqual(nested.returncode, 0)
        self.assertIn("invoking parent", nested.stderr)
        self.assertEqual(self.read(lease), "original")

    def test_invalid_hex_is_rejected_without_partial_appends(self):
        lease = self.create()
        self.append(lease, b"original")
        for bad in ("0", "zz", "ab cd", "aa\nbb", "é0", "１２"):
            with self.subTest(value=bad):
                result = self.command("append", "--lease", json.dumps(lease), "--hex", bad)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("ASCII hex", result.stderr)
                self.assertEqual(result.stdout, "")
        self.assertEqual(self.read(lease), "original")

    def test_incomplete_final_utf8_is_rejected_by_reader(self):
        lease = self.create()
        self.append(lease, b"prefix;\xe2\x82")
        result = child_python("-c", _READER, TOOL, json.dumps(lease))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("UnicodeDecodeError", result.stderr)
        self.append(lease, b"\xac")
        self.assertEqual(self.read(lease), "prefix;€")

    def test_malformed_or_cross_os_descriptors_are_rejected(self):
        for lease in (None, [], {}, {"os": "another-os", "parent_pid": os.getpid()},
                      {"os": os.name, "parent_pid": True},
                      {"os": os.name, "parent_pid": os.getpid(), "handle": True,
                       "token": "../escape", "directory": "../escape", "filename": "records"}):
            with self.subTest(lease=lease):
                result = self.command("release", "--lease", json.dumps(lease))
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertNotIn("Traceback", result.stderr)
        result = self.command("append", "--lease", "not-json", "--hex", "aa")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("Traceback", result.stderr)

    @unittest.skipIf(os.name == "nt", "portable fallback only")
    def test_portable_file_symlink_and_hardlink_substitution_are_rejected(self):
        lease = self.create()
        path = self.portable_path(lease)
        original = path.with_name("original")
        with tempfile.TemporaryDirectory() as temporary:
            victim = Path(temporary) / "victim"
            victim.write_bytes(b"do not read or overwrite")
            victim.chmod(0o600)
            for link in (os.symlink, os.link):
                with self.subTest(link=link.__name__):
                    path.rename(original)
                    try:
                        link(victim, path)
                        result = self.command("append", "--lease", json.dumps(lease), "--hex", "aa")
                        self.assertNotEqual(result.returncode, 0)
                        reader = child_python("-c", _READER, TOOL, json.dumps(lease))
                        self.assertNotEqual(reader.returncode, 0)
                        result = self.command("release", "--lease", json.dumps(lease))
                        self.assertNotEqual(result.returncode, 0)
                        self.assertEqual(victim.read_bytes(), b"do not read or overwrite")
                    finally:
                        path.unlink(missing_ok=True)
                        original.rename(path)
        self.assertEqual(self.read(lease), "")

    @unittest.skipIf(os.name == "nt", "portable fallback only")
    def test_portable_directory_symlink_is_rejected(self):
        lease = self.create()
        directory = self.portable_path(lease).parent
        moved = directory.with_name(directory.name + "-moved")
        directory.rename(moved)
        try:
            directory.symlink_to(moved, target_is_directory=True)
            result = self.command("append", "--lease", json.dumps(lease), "--hex", "aa")
            self.assertNotEqual(result.returncode, 0)
            reader = child_python("-c", _READER, TOOL, json.dumps(lease))
            self.assertNotEqual(reader.returncode, 0)
        finally:
            directory.unlink(missing_ok=True)
            moved.rename(directory)
        self.assertEqual(self.read(lease), "")

    @unittest.skipUnless(os.name == "nt", "requires Windows kernel sharing locks")
    def test_windows_parent_handle_blocks_path_access_and_substitution(self):
        lease = self.create()
        path = windows_handle_path(lease["handle"])
        self.append(lease, b"trusted;manifest")
        # create has exited: only the handle transferred into this process keeps
        # the file alive. Even this same-user process cannot open its pathname.
        for operation in (path.read_bytes, lambda: path.write_bytes(b"tampered"), path.unlink):
            with self.subTest(operation=operation), self.assertRaises(OSError):
                operation()
        with tempfile.TemporaryDirectory() as temporary:
            substitute = Path(temporary) / "substitute"
            substitute.write_bytes(b"tampered")
            with self.assertRaises(OSError):
                os.replace(substitute, path)
            self.assertEqual(substitute.read_bytes(), b"tampered")
        self.assertEqual(self.read(lease), "trusted;manifest")
        self.release(lease)
        self.assertFalse(path.exists())

    @unittest.skipUnless(os.name == "nt", "requires Windows delete-on-close handles")
    def test_windows_parent_exit_cleans_up_unreleased_lease(self):
        owner = """
import ctypes, json, subprocess, sys
from ctypes import wintypes as W
result = subprocess.run([sys.executable, '-I', '-B', sys.argv[1], 'create'],
                        capture_output=True, text=True, check=True)
lease = json.loads(result.stdout)
kernel = ctypes.WinDLL('kernel32', use_last_error=True)
kernel.GetFinalPathNameByHandleW.argtypes = [W.HANDLE, W.LPWSTR, W.DWORD, W.DWORD]
kernel.GetFinalPathNameByHandleW.restype = W.DWORD
buffer = ctypes.create_unicode_buffer(32768)
size = kernel.GetFinalPathNameByHandleW(lease['handle'], buffer, len(buffer), 0)
assert 0 < size < len(buffer)
print(json.dumps(buffer.value))
"""
        result = child_python("-c", owner, TOOL)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(Path(json.loads(result.stdout)).exists())


if __name__ == "__main__":
    unittest.main()
