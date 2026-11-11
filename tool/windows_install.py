"""Resolve companion resources from CMake's structured helper install rules."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

HELPER = "sing_box_flutter_core.exe"


def read_reply(directory: Path, name: str) -> dict:
    if not isinstance(name, str) or Path(name).name != name:
        raise ValueError("Invalid CMake File API reply name")
    return json.loads((directory / name).read_text(encoding="utf-8"))


def manifest_contains(manifest: str, path: Path) -> bool:
    # Compare a known structured path as one scalar. Never split arbitrary paths
    # on semicolons: those characters may belong to a directory name.
    record = path.as_posix()
    manifest = manifest.replace("/./", "/")
    if os.name == "nt":
        record, manifest = record.casefold(), manifest.replace("\\", "/").casefold()
    return ";" + record + ";" in ";" + manifest + ";"


def installation_model(settings: dict, configuration: str) -> tuple[Path, Path, Path, dict]:
    build = Path(settings["build_dir"]).resolve()
    reply = build / ".cmake/api/v1/reply"
    indices = list(reply.glob("index-*.json"))
    if not indices:
        raise ValueError("CMake File API replies are missing; configure with CMake 3.27 or newer")
    index_path = max(indices, key=lambda path: (path.stat().st_mtime_ns, path.name))
    index = json.loads(index_path.read_text(encoding="utf-8"))
    model_reference = max((item for item in index.get("objects", [])
                           if item.get("kind") == "codemodel" and item.get("version", {}).get("major") == 2),
                          key=lambda item: item["version"].get("minor", 0), default=None)
    if model_reference is None:
        raise ValueError("CMake File API codemodel is missing")
    model = read_reply(reply, model_reference["jsonFile"])
    if Path(model["paths"]["build"]).resolve() != build:
        raise ValueError("CMake File API belongs to another build directory")
    source = Path(model["paths"]["source"])
    configurations = model["configurations"]
    selected = next((item for item in configurations if item["name"].casefold() == configuration.casefold()), None)
    if selected is None and len(configurations) == 1 and not configurations[0]["name"]:
        selected = configurations[0]
    if selected is None:
        raise ValueError("CMake install configuration does not match the generated codemodel")
    return build, reply, source, selected


def helper_components(settings: dict, configuration: str) -> tuple[set[str], set[str]]:
    _, reply, source, selected = installation_model(settings, configuration)
    core = (Path(settings["core_dir"]) / HELPER).resolve()
    definite: set[str] = set()
    possible: set[str] = set()
    for directory in selected["directories"]:
        details = read_reply(reply, directory["jsonFile"])
        for installer in details.get("installers", []):
            kind = installer.get("type")
            if kind not in ("file", "directory"):
                continue
            for item in installer.get("paths", []):
                origin = item.get("from") if isinstance(item, dict) else item
                if not isinstance(origin, str):
                    continue
                path = Path(origin)
                path = (path if path.is_absolute() else source / path).resolve()
                component = installer.get("component", "Unspecified")
                if kind == "file" and path == core:
                    if not isinstance(item, dict) or item.get("to", HELPER) == HELPER:
                        definite.add(component)
                        possible.add(component)
                elif kind == "directory" and core.is_relative_to(path):
                    possible.add(component)
    return definite, possible


def install_directories(settings: dict, configuration: str, prefix: str, component: str, *,
                        verify_copies: bool = False, fallback: bool = True,
                        manifest: str | None = None, host_only: bool = False) -> list[str]:
    build, reply, source, selected = installation_model(settings, configuration)
    host = next((item for item in selected.get("targets", [])
                 if item["name"] == settings.get("host_binary")), None)
    host_artifacts = []
    if host_only and host is not None:
        host_artifacts = [(build / item["path"]).resolve()
                          for item in read_reply(reply, host["jsonFile"]).get("artifacts", [])]
    core = (Path(settings["core_dir"]) / HELPER).resolve()
    effective_prefix = settings["fallback_prefix"] if "$<" in prefix else prefix
    destinations: list[Path] = []
    core_digest = None
    for directory in selected["directories"]:
        if "jsonFile" not in directory:
            raise ValueError("CMake File API directory installers are unavailable")
        details = read_reply(reply, directory["jsonFile"])
        for installer in details.get("installers", []):
            kind = installer.get("type")
            if kind not in ("file", "directory", "target"):
                continue
            if component and installer.get("component", "Unspecified") != component:
                continue
            if not component and installer.get("isExcludeFromAll", False):
                continue
            root = Path(installer["destination"])
            if not root.is_absolute():
                root = Path(effective_prefix) / root
            if kind == "target":
                if host_only and host is not None and installer.get("targetId") == host["id"]:
                    destination = root.resolve()
                    if destination not in destinations:
                        destinations.append(destination)
                continue
            for item in installer.get("paths", []):
                origin = item.get("from") if isinstance(item, dict) else item
                if not isinstance(origin, str):
                    continue
                origin_path = Path(origin)
                if not origin_path.is_absolute():
                    origin_path = source / origin_path
                origin_path = origin_path.resolve()
                destination = root
                if host_only:
                    for executable in host_artifacts:
                        if kind == "file":
                            if origin_path != executable:
                                continue
                            placed = destination
                        else:
                            try:
                                relative_executable = executable.relative_to(origin_path)
                            except ValueError:
                                continue
                            to = item.get("to", origin_path.name) if isinstance(item, dict) else origin_path.name
                            placed = destination / to / relative_executable.parent
                            if manifest is None or not manifest_contains(manifest, placed.resolve() / executable.name):
                                continue
                        if placed.resolve() not in destinations:
                            destinations.append(placed.resolve())
                    continue
                if kind == "file":
                    if origin_path != core:
                        continue
                    if isinstance(item, dict) and item.get("to", HELPER) != HELPER:
                        continue
                else:
                    try:
                        relative_core = core.relative_to(origin_path)
                    except ValueError:
                        continue
                    # A trailing-slash directory rule has an explicit `to: .`;
                    # a plain source string retains the source directory name.
                    target_name = item.get("to", origin_path.name) if isinstance(item, dict) else origin_path.name
                    destination /= Path(target_name) / relative_core.parent
                    # File API omits directory filters. Require a complete actual
                    # install record for a directory copy, including files removed
                    # after copying; independently require helper beside each host.
                    if manifest is not None and not manifest_contains(manifest, destination.resolve() / HELPER):
                        continue
                    installed = Path(os.environ.get("DESTDIR", "") + destination.resolve().as_posix()) / HELPER
                    if not verify_copies:
                        if not installed.is_file():
                            continue
                        if core_digest is None:
                            with core.open("rb") as stream:
                                core_digest = hashlib.file_digest(stream, "sha256").digest()
                        with installed.open("rb") as stream:
                            if hashlib.file_digest(stream, "sha256").digest() != core_digest:
                                continue
                destination = destination.resolve()
                if destination not in destinations:
                    destinations.append(destination)
    # Resource-only components have no helper installation in this invocation.
    if not destinations and fallback:
        destinations.append(Path(effective_prefix).resolve())
    return [path.as_posix() for path in destinations]


def verify_installed(settings: dict, configuration: str, prefix: str, component: str,
                     manifest: str | None = None) -> None:
    guard = json.loads(Path(settings["guard_file"]).read_text(encoding="utf-8"))
    destinations = install_directories(settings, configuration, prefix, component,
                                       verify_copies=True, fallback=False, manifest=manifest)
    definite_components, possible_components = helper_components(settings, configuration)
    # An individual component may intentionally stage the host before its helper.
    # Require colocation for whole installs or a component that owns the helper;
    # defer ambiguous directory-filter cases that span other components.
    if (not component or component in definite_components or
            possible_components.issubset({component})):
        destinations += install_directories(settings, configuration, prefix, component,
                                            host_only=True, fallback=False, manifest=manifest)
    destinations = list(dict.fromkeys(destinations))
    invalid = []
    for destination in destinations:
        installed = Path(os.environ.get("DESTDIR", "") + destination) / HELPER
        if installed.is_symlink() or not installed.is_file():
            invalid.append(installed)
            continue
        with installed.open("rb") as stream:
            digest = hashlib.file_digest(stream, "sha256").hexdigest()
        if digest != guard["file_digests"][HELPER]:
            invalid.append(installed)
    if invalid:
        # Never report successful installation of unverified executable bytes.
        for path in invalid:
            if path.is_file() or path.is_symlink():
                path.unlink()
        raise ValueError("Installed Windows helper failed byte verification")
    result = subprocess.run([
        guard["python"], "-I", "-X", "utf8", guard["validator"],
        "--project-dir", guard["project_dir"], "--target", "windows",
        "--windows-arch", guard["architecture"], "--installed-artifact", guard["core_dir"],
        "--installed-sha256", guard["core_digest"],
    ], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        raise ValueError("Windows core selection changed before installation completed")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    records = commands.add_parser("create-records")
    records.add_argument("--directory", type=Path, required=True)
    configure = commands.add_parser("configure")
    configure.add_argument("--output", type=Path, required=True)
    configure.add_argument("--build-dir", type=Path, required=True)
    configure.add_argument("--core-dir", type=Path, required=True)
    configure.add_argument("--fallback-prefix", required=True)
    configure.add_argument("--guard-file", type=Path, required=True)
    configure.add_argument("--host-binary", default="")
    guard = commands.add_parser("guard-settings")
    guard.add_argument("--output", type=Path, required=True)
    guard.add_argument("--project-dir", type=Path, required=True)
    guard.add_argument("--core-dir", type=Path, required=True)
    guard.add_argument("--core-digest", required=True)
    guard.add_argument("--architecture", choices=("amd64", "arm64"), required=True)
    for name in ("resolve", "verify-copies"):
        resolve = commands.add_parser(name)
        resolve.add_argument("--settings", type=Path, required=True)
        resolve.add_argument("--configuration", default="")
        resolve.add_argument("--prefix", required=True)
        resolve.add_argument("--component", default="")
        records = resolve.add_mutually_exclusive_group()
        records.add_argument("--manifest")
        records.add_argument("--manifest-file", type=Path)
    args = parser.parse_args()
    try:
        if args.command == "create-records":
            descriptor, path = tempfile.mkstemp(prefix="sing_box_flutter-installed-", suffix=".txt",
                                               dir=args.directory)
            os.close(descriptor)
            print(json.dumps({"path": Path(path).as_posix()}, ensure_ascii=False))
        elif args.command == "configure":
            settings = {"build_dir": args.build_dir.resolve().as_posix(),
                        "core_dir": args.core_dir.resolve().as_posix(),
                        "fallback_prefix": args.fallback_prefix,
                        "python": Path(sys.executable).as_posix(),
                        "resolver": Path(__file__).resolve().as_posix(),
                        "guard_file": args.guard_file.resolve().as_posix(),
                        "host_binary": args.host_binary}
            args.output.write_text(json.dumps(settings, ensure_ascii=False), encoding="utf-8")
        elif args.command == "guard-settings":
            settings = {"project_dir": args.project_dir.resolve().as_posix(),
                        "core_dir": args.core_dir.resolve().as_posix(),
                        "core_digest": args.core_digest, "architecture": args.architecture,
                        "python": Path(sys.executable).as_posix(),
                        "validator": Path(__file__).with_name("core_recipe.py").resolve().as_posix(),
                        "guard": Path(__file__).with_name("windows_install_guard.py").resolve().as_posix(),
                        "file_digests": json.loads((args.project_dir / ".dart_tool/sing_box_flutter/windows.json")
                                                   .read_text(encoding="utf-8"))["artifacts"]["windows"]["sha256"]}
            args.output.write_text(json.dumps(settings, ensure_ascii=False), encoding="utf-8")
        else:
            settings = json.loads(args.settings.read_text(encoding="utf-8"))
            manifest = args.manifest_file.read_text(encoding="utf-8") if args.manifest_file else args.manifest
            if args.command == "verify-copies":
                verify_installed(settings, args.configuration, args.prefix, args.component, manifest)
            else:
                directories = install_directories(settings, args.configuration, args.prefix, args.component,
                                                   manifest=manifest, fallback=False)
                directories += install_directories(settings, args.configuration, args.prefix, args.component,
                                                    manifest=manifest, host_only=True, fallback=False)
                if not directories:
                    prefix = settings["fallback_prefix"] if "$<" in args.prefix else args.prefix
                    directories = [Path(prefix).resolve().as_posix()]
                print(json.dumps(list(dict.fromkeys(directories)), ensure_ascii=False))
    except (OSError, ValueError, KeyError, TypeError) as error:
        parser.exit(1, f"Windows resource installation: {error}\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
