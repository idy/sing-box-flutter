"""Keep Windows install inputs pinned in the invoking CMake process until exit."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys


def verify_selection(settings: dict) -> None:
    result = subprocess.run([
        settings["python"], "-I", "-X", "utf8", settings["validator"],
        "--project-dir", settings["project_dir"], "--target", "windows",
        "--windows-arch", settings["architecture"], "--installed-artifact", settings["core_dir"],
        "--installed-sha256", settings["core_digest"],
    ], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        raise ValueError("The selected Windows core changed during installation")


class WindowsInputLease:
    """Mandatory kernel sharing locks; never change source ACLs or privileges."""

    def __init__(self, settings: dict, parent_pid: int):
        import ctypes
        from ctypes import wintypes as W
        self.ctypes = ctypes
        self.handle_type = W.HANDLE
        self.kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        self.handles: list[int] = []
        self.kernel.OpenProcess.argtypes = [W.DWORD, W.BOOL, W.DWORD]
        self.kernel.OpenProcess.restype = W.HANDLE
        self.kernel.CreateFileW.argtypes = [W.LPCWSTR, W.DWORD, W.DWORD, W.LPVOID,
                                            W.DWORD, W.DWORD, W.HANDLE]
        self.kernel.CreateFileW.restype = W.HANDLE
        self.kernel.CloseHandle.argtypes = [W.HANDLE]
        self.kernel.CloseHandle.restype = W.BOOL
        self.kernel.GetCurrentProcess.restype = W.HANDLE
        self.kernel.DuplicateHandle.argtypes = [W.HANDLE, W.HANDLE, W.HANDLE,
                                                ctypes.POINTER(W.HANDLE), W.DWORD, W.BOOL, W.DWORD]
        self.kernel.DuplicateHandle.restype = W.BOOL
        class FileInfo(ctypes.Structure):
            _fields_ = [("attributes", W.DWORD), ("created", W.FILETIME),
                        ("accessed", W.FILETIME), ("written", W.FILETIME),
                        ("volume", W.DWORD), ("size_high", W.DWORD), ("size_low", W.DWORD),
                        ("links", W.DWORD), ("index_high", W.DWORD), ("index_low", W.DWORD)]
        self.info_type = FileInfo
        self.kernel.GetFileInformationByHandle.argtypes = [W.HANDLE, ctypes.POINTER(FileInfo)]
        self.kernel.GetFileInformationByHandle.restype = W.BOOL
        self.parent = None
        try:
            self.parent = self.kernel.OpenProcess(0x0040, False, parent_pid)  # PROCESS_DUP_HANDLE
            if not self.parent:
                raise ValueError("Could not retain the invoking installer process")
            parent = Path(settings["core_dir"]).parent
            paths = {Path(settings["project_dir"]) / "pubspec.yaml",
                     Path(settings["project_dir"]) / ".dart_tool/sing_box_flutter/windows.json"}
            entries = list(parent.rglob("*"))
            if len(entries) > 4096:
                raise ValueError("Too many Windows artifact inputs")
            paths.update(entries)
            paths.add(parent)
            all_paths = set(paths)
            for path in paths:
                all_paths.update(path.parents)
            # Pin directory entries first. Verification runs only after every
            # source/receipt pin is held, preventing path swaps during admission.
            for path in sorted(all_paths, key=lambda item: (len(item.parts), str(item))):
                self.pin(path)
        except BaseException:
            self.close()
            raise

    def pin(self, path: Path) -> None:
        if len(path.drive) != 2 or path.drive[1] != ":" or not path.is_absolute():
            raise ValueError("Installer inputs must use local absolute paths")
        handle = self.kernel.CreateFileW(
            str(path), 0x80000000, 1, None, 3,
            0x02000000 | 0x00200000, None,  # backup semantics + open reparse point
        )
        if handle in (None, self.ctypes.c_void_p(-1).value):
            raise ValueError("Could not protect Windows install inputs")
        self.handles.append(handle)
        info = self.info_type()
        if not self.kernel.GetFileInformationByHandle(handle, self.ctypes.byref(info)):
            raise ValueError("Could not inspect a Windows install input")
        if info.attributes & 0x400:
            raise ValueError("Windows installer input contains a reparse point")
        if not info.attributes & 0x10 and info.links != 1:
            raise ValueError("Windows installer input has multiple hard links")

    def transfer_to_installer(self) -> None:
        for handle in self.handles:
            transferred = self.handle_type()
            if not self.kernel.DuplicateHandle(self.kernel.GetCurrentProcess(), handle, self.parent,
                                                self.ctypes.byref(transferred), 0, False, 2):
                raise ValueError("Could not transfer input protection to the installer")
        # CMake owns the duplicate handles until process exit. Its kernel handle
        # table releases them even on a failed install; no daemon, writable IPC,
        # token privilege change or cooperative lock is involved.

    def close(self) -> None:
        for handle in reversed(self.handles):
            self.kernel.CloseHandle(handle)
        self.handles.clear()
        if self.parent:
            self.kernel.CloseHandle(self.parent)
            self.parent = None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--settings", type=Path, required=True)
    args = parser.parse_args()
    lease = None
    try:
        settings = json.loads(args.settings.read_text(encoding="utf-8"))
        if os.name == "nt":
            lease = WindowsInputLease(settings, os.getppid())
        verify_selection(settings)
        if lease is not None:
            lease.transfer_to_installer()
    except (OSError, ValueError, KeyError, TypeError) as error:
        parser.exit(1, f"Windows install-time verification: {error}\n")
    finally:
        if lease is not None:
            lease.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
