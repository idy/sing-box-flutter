"""Selected Windows helper overlay and its pinned sing-tun embedded Wintun driver."""
from __future__ import annotations

import contextlib
import hashlib
import json
from pathlib import Path
import re
import shutil
import struct

from native_common import BuildError, ROOT, selected_sources, sha256

WINDOWS_ARCHES = {"amd64": 0x8664, "arm64": 0xAA64}
WINDOWS_EXECUTABLE = "sing_box_flutter_core.exe"
WINDOWS_SHARED_SOURCES = ("constructor_cleanup.go", "capabilities.go", "capabilities_alpn_shared_test.go")
WINTUN_MODULE = {
    "path": "github.com/sagernet/sing-tun",
    "version": "v0.9.7-0.20261023115432-040db71fcd33",
    "sum": "h1:0+op+qbxh/M/JsqIG8tkEwPM+5pZtBeicf1ciMxp4OI=",
}


def load_wintun_lock(root: Path = ROOT) -> dict:
    try:
        lock = json.loads((root / "tool/wintun.json").read_text(encoding="utf-8"))
        version = lock["version"]
        if (lock["schema"] != 1 or not re.fullmatch(r"\d+\.\d+\.\d+", version) or
                lock["url"] != f"https://www.wintun.net/builds/wintun-{version}.zip" or
                lock["checksum_source"] != "https://www.wintun.net/" or
                not re.fullmatch(r"[0-9a-f]{64}", lock["sha256"]) or
                lock["module"] != WINTUN_MODULE or
                set(lock["files"]) != {*WINDOWS_ARCHES, "license"}):
            raise ValueError("invalid lock")
        for name, record in lock["files"].items():
            expected = "wintun/LICENSE.txt" if name == "license" else f"wintun/bin/{name}/wintun.dll"
            if record["path"] != expected or not re.fullmatch(r"[0-9a-f]{64}", record["sha256"]):
                raise ValueError("invalid file lock")
        return lock
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise BuildError("The pinned official Wintun checksum lock is missing or invalid") from error


def windows_sources(root: Path = ROOT) -> dict[str, str]:
    sources = selected_sources(root, "windows")
    shared = root / "native/mobile"
    if not shared.is_dir() or shared.is_symlink():
        raise BuildError("The shared selectable core source is missing")
    for name in WINDOWS_SHARED_SOURCES:
        path = shared / name
        if path.is_symlink() or not path.is_file() or name in sources:
            raise BuildError("The shared selectable core source is missing or duplicated")
        # Identity covers canonical raw inputs, before the package header rewrite.
        sources[f"mobile/{name}"] = sha256(path)
    return sources


@contextlib.contextmanager
def windows_overlay(source: Path, registry: str, expected_sources: dict[str, str], root: Path = ROOT):
    """Add only our cmd package; always remove it before upstream integrity checks."""
    if windows_sources(root) != expected_sources:
        raise BuildError("Windows core source changed after the build identity was captured")
    destination = source / "cmd/flutter_windows"
    if destination.exists() or destination.is_symlink():
        raise BuildError("The pinned core already contains cmd/flutter_windows; remove the cached checkout and retry")
    destination.mkdir(parents=True)
    try:
        for name, digest in expected_sources.items():
            if name in ("go.mod", "go.sum"):
                continue
            shared = name.startswith("mobile/")
            path = root / "native" / (name if shared else f"windows/{name}")
            content = path.read_bytes()
            if hashlib.sha256(content).hexdigest() != digest:
                raise BuildError("Windows core source changed while capturing the build")
            if shared:
                content, count = re.subn(rb"(?m)^package libbox(?=\r?$)", b"package main", content)
                if count != 1:
                    raise BuildError("The shared selectable core package header is invalid")
            output = destination / (Path(name).name if shared else name)
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_bytes(content)
        (destination / "registry_selected.go").write_text(registry, encoding="utf-8")
        yield
    finally:
        shutil.rmtree(destination)


def windows_command(go: Path, output: Path, arch: str, tags: list[str], pins: dict) -> list:
    if arch not in WINDOWS_ARCHES:
        raise BuildError("Windows architecture must be amd64 or arm64")
    linker = ("-s -w -buildid= -checklinkname=0 "
              f"-X github.com/sagernet/sing-box/constant.Version={pins['sing_box_version']}")
    return [go, "build", "-trimpath", "-buildvcs=false", "-ldflags", linker,
            "-tags", ",".join(sorted(set(tags) | {"badlinkname", "tfogo_checklinkname0"})),
            "-o", output, "./cmd/flutter_windows"]


def resolve_windows_test_graphs(go: Path, env: dict, source: Path, run, *,
                               arch: str, tags: list[str]) -> dict:
    """Read only the selected host/target test graphs, without compiling or execution.

    Do not add -compiled/-export or download all go.mod entries: unused Cronet
    variants alone add gigabytes to a default Windows build. The caller must
    verify resolved modules before executing tests or compiling the helper.
    """
    if arch not in WINDOWS_ARCHES:
        raise BuildError("Windows architecture must be amd64 or arm64")
    host_env = {key: value for key, value in env.items() if key not in ("GOOS", "GOARCH")}
    host_env["CGO_ENABLED"] = "0"
    windows_env = {**env, "GOOS": "windows", "GOARCH": arch, "CGO_ENABLED": "0"}
    command = [go, "list", "-deps", "-test", "-tags", ",".join(tags), "./cmd/flutter_windows"]
    run(command, cwd=source, env=host_env, capture=True)
    run(command, cwd=source, env=windows_env, capture=True)
    return windows_env


def prepare_windows_dependencies(go: Path, env: dict, source: Path, arch: str,
                                 tags: list[str], run) -> dict:
    """Resolve and verify the selected graphs when no WGC overlay owns verification."""
    windows_env = resolve_windows_test_graphs(go, env, source, run, arch=arch, tags=tags)
    run([go, "mod", "verify"], cwd=source, env=windows_env)
    return windows_env


def pe_machine(path: Path) -> int:
    """Read only the DOS/PE signature and COFF architecture; never execute it."""
    try:
        if path.is_symlink():
            raise ValueError("symlink")
        with path.open("rb") as stream:
            header = stream.read(64)
            if len(header) != 64 or header[:2] != b"MZ":
                raise ValueError("not DOS/PE")
            offset = struct.unpack_from("<I", header, 60)[0]
            if not 64 <= offset <= 1024 * 1024:
                raise ValueError("invalid PE offset")
            stream.seek(offset)
            signature = stream.read(6)
            if len(signature) != 6 or signature[:4] != b"PE\0\0":
                raise ValueError("not PE/COFF")
            return struct.unpack_from("<H", signature, 4)[0]
    except (OSError, ValueError, struct.error) as error:
        raise BuildError("The Windows core bundle contains an invalid PE executable or DLL") from error


def validate_windows_bundle(bundle: Path, arch: str | None = None, lock: dict | None = None) -> None:
    if lock is None:
        lock = load_wintun_lock()
    if bundle.is_symlink() or not bundle.is_dir() or any(path.is_symlink() for path in bundle.rglob("*")):
        raise BuildError("The Windows core bundle must be a regular directory without symlinks")
    if {path.name for path in bundle.iterdir()} != {WINDOWS_EXECUTABLE, "LICENSE.wintun"}:
        raise BuildError("The Windows bundle must contain the helper and embedded Wintun license only")
    machine = pe_machine(bundle / WINDOWS_EXECUTABLE)
    if arch is None:
        arch = next((name for name, value in WINDOWS_ARCHES.items() if value == machine), None)
    if arch not in WINDOWS_ARCHES or machine != WINDOWS_ARCHES[arch]:
        raise BuildError("Windows helper must match the selected amd64/arm64 architecture")
    if sha256(bundle / "LICENSE.wintun") != lock["files"]["license"]["sha256"]:
        raise BuildError("The Windows bundle Wintun license differs from the pinned official release")


def verify_embedded_wintun(go: Path, env: dict, source: Path, arch: str, lock: dict, run) -> dict:
    """Bind provenance to the actual target module/resource used by sing-tun memmod."""
    try:
        if arch not in WINDOWS_ARCHES or lock["module"] != WINTUN_MODULE:
            raise ValueError("invalid embedded driver lock")
        module = json.loads(run([go, "list", "-m", "-json", WINTUN_MODULE["path"]],
                                cwd=source, env=env, capture=True))
        if (module.get("Replace") is not None or module.get("Main") or module.get("Error") or
                any(module.get(key.capitalize()) != value for key, value in WINTUN_MODULE.items())):
            raise ValueError("unexpected module identity")
        directory = Path(module["Dir"])
        if not directory.is_absolute():
            raise ValueError("module directory is not absolute")
        resource = f"internal/wintun/{arch}/wintun.dll"
        path = directory / resource
        if (any(part.is_symlink() for part in (path, *path.parents)) or
                not path.is_file() or sha256(path) != lock["files"][arch]["sha256"] or
                pe_machine(path) != WINDOWS_ARCHES[arch]):
            raise ValueError("unexpected embedded driver")
    except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
        raise BuildError("The selected sing-tun module or embedded Wintun driver differs from the pinned lock") from error
    # Keep only authenticated identities and a relative resource, never cache or
    # private replacement paths returned by go list.
    return {"version": lock["version"], "url": lock["url"], "sha256": lock["sha256"],
            "arch": arch, "dll_sha256": lock["files"][arch]["sha256"],
            "license_sha256": lock["files"]["license"]["sha256"],
            "module": dict(WINTUN_MODULE), "embedded_resource": resource, "loading": "embedded-memmod"}


def build_windows_bundle(go: Path, env: dict, source: Path, output: Path, arch: str,
                         tags: list[str], pins: dict, lock: dict, run, *, root: Path = ROOT) -> None:
    """Compile after dependency preparation; retain the package-owned release license."""
    license_path = root / "native/windows/LICENSE.wintun"
    if (license_path.is_symlink() or not license_path.is_file() or
            sha256(license_path) != lock["files"]["license"]["sha256"]):
        raise BuildError("The package-owned Wintun license differs from the pinned official release")
    output.mkdir()
    windows_env = {**env, "GOOS": "windows", "GOARCH": arch, "CGO_ENABLED": "0"}
    run(windows_command(go, output / WINDOWS_EXECUTABLE, arch, tags, pins), cwd=source, env=windows_env)
    shutil.copyfile(license_path, output / "LICENSE.wintun")
    validate_windows_bundle(output, arch, lock)
