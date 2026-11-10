"""Resolve companion resources from CMake's structured helper install rules."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import sys

HELPER = "sing_box_flutter_core.exe"


def read_reply(directory: Path, name: str) -> dict:
    if not isinstance(name, str) or Path(name).name != name:
        raise ValueError("Invalid CMake File API reply name")
    return json.loads((directory / name).read_text(encoding="utf-8"))


def install_directories(settings: dict, configuration: str, prefix: str, component: str) -> list[str]:
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
            if kind not in ("file", "directory"):
                continue
            if component and installer.get("component", "Unspecified") != component:
                continue
            if not component and installer.get("isExcludeFromAll", False):
                continue
            for item in installer.get("paths", []):
                origin = item.get("from") if isinstance(item, dict) else item
                if not isinstance(origin, str):
                    continue
                origin_path = Path(origin)
                if not origin_path.is_absolute():
                    origin_path = source / origin_path
                origin_path = origin_path.resolve()
                destination = Path(installer["destination"])
                if not destination.is_absolute():
                    destination = Path(effective_prefix) / destination
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
                    # Directory filters may exclude the helper. Only accompany
                    # an actual copy of the verified binary, including DESTDIR.
                    installed = Path(os.environ.get("DESTDIR", "") + destination.resolve().as_posix()) / HELPER
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
    if not destinations:
        destinations.append(Path(effective_prefix).resolve())
    return [path.as_posix() for path in destinations]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    configure = commands.add_parser("configure")
    configure.add_argument("--output", type=Path, required=True)
    configure.add_argument("--build-dir", type=Path, required=True)
    configure.add_argument("--core-dir", type=Path, required=True)
    configure.add_argument("--fallback-prefix", required=True)
    resolve = commands.add_parser("resolve")
    resolve.add_argument("--settings", type=Path, required=True)
    resolve.add_argument("--configuration", default="")
    resolve.add_argument("--prefix", required=True)
    resolve.add_argument("--component", default="")
    args = parser.parse_args()
    try:
        if args.command == "configure":
            settings = {"build_dir": args.build_dir.resolve().as_posix(),
                        "core_dir": args.core_dir.resolve().as_posix(),
                        "fallback_prefix": args.fallback_prefix,
                        "python": Path(sys.executable).as_posix(),
                        "resolver": Path(__file__).resolve().as_posix()}
            args.output.write_text(json.dumps(settings, ensure_ascii=False), encoding="utf-8")
        else:
            settings = json.loads(args.settings.read_text(encoding="utf-8"))
            print(json.dumps(install_directories(settings, args.configuration, args.prefix, args.component),
                             ensure_ascii=False))
    except (OSError, ValueError, KeyError, TypeError) as error:
        parser.exit(1, f"Windows resource installation: {error}\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
