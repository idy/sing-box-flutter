"""Transport CMake's UTF-8 install records without reopening Windows pathnames.

CLI: create [--directory UNUSED], append --lease JSON --hex ASCII_HEX,
release --lease JSON. Only create writes stdout (one opaque JSON descriptor).
read_records(json.loads(descriptor)) is for an installer child of the same parent.
Hex chunks are bytes; a chunk may end inside a UTF-8 codepoint.
"""
from __future__ import annotations

import argparse
from contextlib import contextmanager
import json
import os
from pathlib import Path
import secrets
import stat
import tempfile


_PREFIX = "sing-box-install-records-"
_FILENAME = "records"
_IO_CHUNK = 65536


def _validate_lease(lease: dict) -> None:
    if not isinstance(lease, dict) or lease.get("os") != os.name:
        raise ValueError("Install-record lease belongs to another operating system")
    parent = lease.get("parent_pid")
    if type(parent) is not int or parent <= 0 or parent != os.getppid():
        raise ValueError("Install-record lease does not belong to the invoking parent")
    if os.name == "nt":
        import struct
        handle = lease.get("handle")
        limit = (1 << (8 * struct.calcsize("P"))) - 1
        if type(handle) is not int or not 0 < handle < limit:
            raise ValueError("Invalid install-record handle")
    else:
        token = lease.get("token")
        if (not isinstance(token, str) or len(token) != 64
                or any(character not in "0123456789abcdef" for character in token)
                or lease.get("directory") != _PREFIX + token
                or lease.get("filename") != _FILENAME):
            raise ValueError("Invalid private install-record location")


class _WindowsHandles:
    """All record I/O uses a duplicate of the handle held by CMake."""

    def __init__(self):
        import ctypes
        from ctypes import wintypes as W
        self.ctypes = ctypes
        self.handle_type = W.HANDLE
        self.kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        self.kernel.OpenProcess.argtypes = [W.DWORD, W.BOOL, W.DWORD]
        self.kernel.OpenProcess.restype = W.HANDLE
        self.kernel.GetCurrentProcess.argtypes = []
        self.kernel.GetCurrentProcess.restype = W.HANDLE
        self.kernel.CloseHandle.argtypes = [W.HANDLE]
        self.kernel.CloseHandle.restype = W.BOOL
        self.kernel.CreateFileW.argtypes = [W.LPCWSTR, W.DWORD, W.DWORD, W.LPVOID,
                                            W.DWORD, W.DWORD, W.HANDLE]
        self.kernel.CreateFileW.restype = W.HANDLE
        self.kernel.DuplicateHandle.argtypes = [W.HANDLE, W.HANDLE, W.HANDLE,
                                                ctypes.POINTER(W.HANDLE), W.DWORD,
                                                W.BOOL, W.DWORD]
        self.kernel.DuplicateHandle.restype = W.BOOL
        self.kernel.SetFilePointerEx.argtypes = [W.HANDLE, ctypes.c_longlong,
                                                 ctypes.POINTER(ctypes.c_longlong), W.DWORD]
        self.kernel.SetFilePointerEx.restype = W.BOOL
        self.kernel.ReadFile.argtypes = [W.HANDLE, W.LPVOID, W.DWORD,
                                        ctypes.POINTER(W.DWORD), W.LPVOID]
        self.kernel.ReadFile.restype = W.BOOL
        self.kernel.WriteFile.argtypes = self.kernel.ReadFile.argtypes
        self.kernel.WriteFile.restype = W.BOOL
        self.kernel.GetFileType.argtypes = [W.HANDLE]
        self.kernel.GetFileType.restype = W.DWORD
        self.dword_type = W.DWORD

    def _error(self, operation: str) -> OSError:
        error = self.ctypes.WinError(self.ctypes.get_last_error())
        return OSError(f"Could not {operation} install records: {error}")

    def close(self, handle: int) -> None:
        self.kernel.CloseHandle(handle)

    @contextmanager
    def parent(self, parent_pid: int):
        # PROCESS_DUP_HANDLE only; do not enable privileges or accept any PID
        # other than our real parent (validated before this call).
        handle = self.kernel.OpenProcess(0x0040, False, parent_pid)
        if not handle:
            raise self._error("access the invoking parent's")
        try:
            yield handle
        finally:
            self.close(handle)

    def duplicate(self, source_process: int, handle: int, target_process: int, *,
                  close_source: bool = False) -> int:
        result = self.handle_type()
        options = 2 | (1 if close_source else 0)  # SAME_ACCESS | CLOSE_SOURCE
        if not self.kernel.DuplicateHandle(source_process, handle, target_process,
                                            self.ctypes.byref(result), 0, False, options):
            raise self._error("duplicate")
        return result.value

    def create(self, parent_pid: int) -> dict:
        with self.parent(parent_pid) as parent:
            for _ in range(16):
                path = Path(tempfile.gettempdir()) / (_PREFIX + secrets.token_hex(32))
                handle = self.kernel.CreateFileW(
                    str(path), 0x80000000 | 0x40000000 | 0x00010000,
                    0, None, 1, 0x00200000 | 0x04000000, None,
                    # READ | WRITE | DELETE; share=0; CREATE_NEW;
                    # OPEN_REPARSE_POINT | DELETE_ON_CLOSE.
                )
                if handle not in (None, self.ctypes.c_void_p(-1).value):
                    break
                if self.ctypes.get_last_error() not in (80, 183):
                    raise self._error("create")
            else:
                raise OSError("Could not allocate unique install records")
            try:
                remote = self.duplicate(self.kernel.GetCurrentProcess(), handle, parent)
            finally:
                # The parent duplicate retains the file and its sharing lock.
                self.close(handle)
        return {"os": "nt", "parent_pid": parent_pid, "handle": remote}

    @contextmanager
    def record(self, lease: dict, *, release: bool = False):
        with self.parent(lease["parent_pid"]) as parent:
            handle = self.duplicate(parent, lease["handle"], self.kernel.GetCurrentProcess(),
                                    close_source=release)
        try:
            if self.kernel.GetFileType(handle) != 1:  # FILE_TYPE_DISK
                raise ValueError("Install-record handle is not a file")
            yield handle
        finally:
            self.close(handle)

    def seek(self, handle: int, origin: int) -> None:
        if not self.kernel.SetFilePointerEx(handle, 0, None, origin):
            raise self._error("seek")

    def append(self, handle: int, data: bytes) -> None:
        self.seek(handle, 2)  # FILE_END; duplicates share a file pointer.
        for start in range(0, len(data), _IO_CHUNK):
            chunk = data[start:start + _IO_CHUNK]
            offset = 0
            while offset < len(chunk):
                remaining = chunk[offset:]
                buffer = self.ctypes.create_string_buffer(remaining)
                written = self.dword_type()
                if not self.kernel.WriteFile(handle, buffer, len(remaining),
                                             self.ctypes.byref(written), None):
                    raise self._error("write")
                if not 0 < written.value <= len(remaining):
                    raise OSError("Could not make progress writing install records")
                offset += written.value

    def read(self, handle: int) -> bytes:
        self.seek(handle, 0)  # FILE_BEGIN, including after an earlier read.
        chunks = []
        buffer = self.ctypes.create_string_buffer(_IO_CHUNK)
        while True:
            count = self.dword_type()
            if not self.kernel.ReadFile(handle, buffer, _IO_CHUNK,
                                        self.ctypes.byref(count), None):
                raise self._error("read")
            if not count.value:
                return b"".join(chunks)
            chunks.append(buffer.raw[:count.value])


def _private_stat(info, *, directory: bool) -> None:
    expected = 0o700 if directory else 0o600
    kind = stat.S_ISDIR if directory else stat.S_ISREG
    if (not kind(info.st_mode) or stat.S_IMODE(info.st_mode) != expected
            or info.st_uid != os.geteuid() or (not directory and info.st_nlink != 1)):
        raise ValueError("Install-record storage is not a private regular file/directory")


def _same_file(before, after) -> None:
    if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
        raise ValueError("Install-record storage changed while opening")


def _portable_path(lease: dict) -> Path:
    return Path(tempfile.gettempdir()) / lease["directory"]


@contextmanager
def _portable_directory(lease: dict):
    path = _portable_path(lease)
    before = os.stat(path, follow_symlinks=False)
    _private_stat(before, directory=True)
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    handle = os.open(path, flags)
    try:
        after = os.fstat(handle)
        _private_stat(after, directory=True)
        _same_file(before, after)
        yield handle
    finally:
        os.close(handle)


@contextmanager
def _portable_record(directory: int, flags: int):
    before = os.stat(_FILENAME, dir_fd=directory, follow_symlinks=False)
    _private_stat(before, directory=False)
    handle = os.open(_FILENAME, flags | getattr(os, "O_NOFOLLOW", 0)
                     | getattr(os, "O_NONBLOCK", 0), dir_fd=directory)
    try:
        after = os.fstat(handle)
        _private_stat(after, directory=False)
        _same_file(before, after)
        yield handle
    finally:
        os.close(handle)


def create_records() -> dict:
    """Create a lease for children of the invoking parent, never in a build tree."""
    parent_pid = os.getppid()
    if os.name == "nt":
        return _WindowsHandles().create(parent_pid)
    for _ in range(16):
        token = secrets.token_hex(32)
        lease = {"os": os.name, "parent_pid": parent_pid, "token": token,
                 "directory": _PREFIX + token, "filename": _FILENAME}
        path = _portable_path(lease)
        try:
            os.mkdir(path, 0o700)
        except FileExistsError:
            continue
        try:
            with _portable_directory(lease) as directory:
                handle = os.open(_FILENAME, os.O_CREAT | os.O_EXCL | os.O_RDWR
                                 | getattr(os, "O_NOFOLLOW", 0), 0o600, dir_fd=directory)
                try:
                    os.fchmod(handle, 0o600)
                finally:
                    os.close(handle)
        except BaseException:
            # Creation cannot have handed the location to anyone yet.
            (path / _FILENAME).unlink(missing_ok=True)
            path.rmdir()
            raise
        return lease
    raise OSError("Could not allocate unique install records")


def append_records(lease: dict, data: bytes) -> None:
    _validate_lease(lease)
    if os.name == "nt":
        windows = _WindowsHandles()
        with windows.record(lease) as handle:
            windows.append(handle, data)
        return
    with _portable_directory(lease) as directory, _portable_record(directory, os.O_WRONLY) as handle:
        os.lseek(handle, 0, os.SEEK_END)
        view = memoryview(data)
        while view:
            count = os.write(handle, view[:_IO_CHUNK])
            if count <= 0:
                raise OSError("Could not make progress writing install records")
            view = view[count:]


def read_records(lease: dict) -> str:
    """Read exact UTF-8 records in another child of the lease's original parent."""
    _validate_lease(lease)
    if os.name == "nt":
        windows = _WindowsHandles()
        with windows.record(lease) as handle:
            return windows.read(handle).decode("utf-8")
    with _portable_directory(lease) as directory, _portable_record(directory, os.O_RDONLY) as handle:
        os.lseek(handle, 0, os.SEEK_SET)
        chunks = []
        while True:
            chunk = os.read(handle, _IO_CHUNK)
            if not chunk:
                return b"".join(chunks).decode("utf-8")
            chunks.append(chunk)


def release_records(lease: dict) -> None:
    _validate_lease(lease)
    if os.name == "nt":
        windows = _WindowsHandles()
        # CLOSE_SOURCE closes CMake's remote copy; closing our local duplicate
        # releases the last reference and triggers DELETE_ON_CLOSE immediately.
        with windows.record(lease, release=True):
            pass
        return
    with _portable_directory(lease) as directory, _portable_record(directory, os.O_RDONLY):
        os.unlink(_FILENAME, dir_fd=directory)
    _portable_path(lease).rmdir()


def _hex_bytes(value: str) -> bytes:
    # bytes.fromhex alone accepts whitespace; CMake sends ASCII hex only.
    if len(value) % 2 or any(character not in "0123456789abcdefABCDEF" for character in value):
        raise ValueError("Install records must be an even number of ASCII hex digits")
    return bytes.fromhex(value)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    create = commands.add_parser("create")
    create.add_argument("--directory", help="Compatibility option; storage always uses system temp")
    append = commands.add_parser("append")
    append.add_argument("--lease", required=True)
    append.add_argument("--hex", required=True)
    release = commands.add_parser("release")
    release.add_argument("--lease", required=True)
    args = parser.parse_args()
    try:
        if args.command == "create":
            print(json.dumps(create_records(), separators=(",", ":")))
        elif args.command == "append":
            append_records(json.loads(args.lease), _hex_bytes(args.hex))
        else:
            release_records(json.loads(args.lease))
    except (OSError, ValueError, TypeError, KeyError) as error:
        parser.exit(1, f"Windows install records: {error}\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
