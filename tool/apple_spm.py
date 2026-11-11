#!/usr/bin/env python3
"""Stage and guard a host-private Swift package for a selected Apple core."""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile

sys.dont_write_bytecode = True
# Isolated Python omits siblings. Restore only this trusted tooling directory.
sys.path.insert(0, str(Path(__file__).resolve().parent))
from native_common import BuildError, ROOT

NAME = "SingBoxFlutterHost"
TEMPLATE = "tool/templates/apple_spm/Validate.swift"
PRIVACY = "ios/sing_box_flutter/Sources/sing_box_flutter/PrivacyInfo.xcprivacy"
CONTROLLER = "ios/sing_box_flutter/Sources/sing_box_flutter/SingBoxFlutterPlugin.swift"


def _json(value: object) -> bytes:
    return (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()


def _hash(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _swift(value: str) -> str:
    # JSON string escapes overlap Swift except Unicode escapes; preserve Unicode.
    return json.dumps(value, ensure_ascii=False).replace('\\/', '/')


def _safe_path(project: Path, relative: Path) -> Path:
    if relative.is_absolute() or ".." in relative.parts:
        raise BuildError("Invalid host-local Swift package path")
    path = project
    for part in relative.parts:
        path /= part
        if path.is_symlink():
            raise BuildError("Host-local Swift package paths must not traverse symlinks")
    return path


def _package(project: Path, target: str) -> Path:
    if target not in ("apple", "macos"):
        raise BuildError("Swift package staging supports only apple and macos")
    platform = "ios" if target == "apple" else "macos"
    return _safe_path(project, Path(platform) / "Flutter/ephemeral/Packages" / NAME)


def _receipt(project: Path) -> dict:
    from build_core import RECEIPT, host_path
    return json.loads(host_path(project, RECEIPT.as_posix()).read_text(encoding="utf-8"))


def _manifest(target: str, mode: str, digest: str) -> bytes:
    ne = mode == "ne"
    common = ['.define("SINGBOX_SWIFT_PACKAGE")', '.define("SINGBOX_SELECTABLE_CORE")']
    links = ['.linkedFramework("' + name + '")' for name in
             ("Network", "Security", "SystemConfiguration", "CoreFoundation")]
    links += ['.linkedLibrary("resolv")']
    if ne:
        links += ['.linkedFramework("NetworkExtension")']
    if target == "apple":
        links += ['.linkedFramework("UIKit")']

    def swift_target(name: str, dependencies: list[str], extension: bool = False) -> str:
        settings = common + (['.unsafeFlags(["-application-extension"])'] if extension else [])
        linker = links if extension or not ne else ['.linkedFramework("NetworkExtension")']
        return f'''        .target(name: "{name}", dependencies: [{', '.join(dependencies)}],
                resources: [.process("PrivacyInfo.xcprivacy")],
                swiftSettings: [{', '.join(settings)}],
                linkerSettings: [{', '.join(linker)}],
                plugins: [.plugin(name: "SingBoxFlutterValidate")])'''

    products = ['.library(name: "SingBoxFlutterHost", targets: ["SingBoxFlutterHost"])']
    targets = [swift_target(NAME, ['.product(name: "FlutterFramework", package: "FlutterFramework")'] +
                            ([] if ne else ['"Libbox"']))]
    if ne:
        products.append('.library(name: "sing-box-flutter-packet-tunnel", targets: ["SingBoxFlutterPacketTunnel"])')
        targets.append(swift_target("SingBoxFlutterPacketTunnel", ['"Libbox"'], True))
    targets += [f'.binaryTarget(name: "Libbox", path: "Artifacts/{digest}/Libbox.xcframework")',
                '.plugin(name: "SingBoxFlutterValidate", capability: .buildTool(), path: "Plugins/Validate")']
    return ('''// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "SingBoxFlutterHost",
    platforms: [.iOS(.v15), .macOS(.v12)],
    products: [\n        ''' + ',\n        '.join(products) + '''],
    dependencies: [.package(name: "FlutterFramework", path: "../.packages/FlutterFramework")],
    targets: [\n''' + ',\n'.join(targets) + '\n    ]\n)\n').encode()


def _snapshot(project: Path, receipt: dict, target: str, root: Path,
              python: str, installed: tuple[str, str] | None = None) -> tuple[dict, dict[str, bytes], Path]:
    from build_core import canonical_macos, host_path
    from core_recipe import verify_current_artifact, verify_current_recipe
    try:
        entry = receipt["artifacts"][target]
    except (KeyError, TypeError) as error:
        raise BuildError("Swift packages require a selected core; build this host's Apple target first") from error
    metadata = json.loads(host_path(project, entry["metadata"]).read_text(encoding="utf-8"))
    verify_current_recipe(project, receipt, metadata, target=target, root=root)
    digest = verify_current_artifact(project, receipt, target, installed)
    mode = canonical_macos(receipt.get("macos", {}))["mode"] if target == "macos" else "ne"
    artifact = host_path(project, entry["path"])
    if artifact.name != "Libbox.xcframework" or not artifact.is_dir():
        raise BuildError("Selected Apple core must be a Libbox.xcframework directory")
    shared = sorted((root / "ios/Classes/Shared").glob("*.swift"))
    if not shared:
        raise BuildError("Canonical Apple Swift sources are missing")
    if mode == "ne":
        sources = {NAME: shared + [root / CONTROLLER],
                   "SingBoxFlutterPacketTunnel": shared + sorted((root / "ios/Classes/PacketTunnel").glob("*.swift"))}
    else:
        sources = {NAME: [p for p in shared if p.name != "ConfigPreflight.swift"] +
                   sorted((root / "macos/Classes/UserTun").glob("*.swift")) +
                   [root / "ios/Classes/PacketTunnel/NativeCore.swift"]}
    files = {}
    canonical = {}
    for module, paths in sources.items():
        for source in paths + [root / PRIVACY]:
            data = source.read_bytes()
            destination = f"Sources/{module}/{source.name}"
            if destination in files:
                raise BuildError("Duplicate Swift package source filename")
            files[destination] = data
            canonical[source.relative_to(root).as_posix()] = _hash(data)
    platform = "ios" if target == "apple" else "macos"
    # Include the static facade, all its sources, and the validator itself.
    facade = root / platform / "sing_box_flutter"
    inputs = [root / TEMPLATE, root / "tool/apple_spm.py", facade / "Package.swift"]
    inputs += sorted((facade / "Sources").rglob("*.swift"))
    for source in inputs:
        canonical[source.relative_to(root).as_posix()] = _hash(source.read_bytes())
    template = (root / TEMPLATE).read_text(encoding="utf-8")
    files["Plugins/Validate/Validate.swift"] = template.replace("@@PYTHON@@", _swift(python)).replace(
        "@@VALIDATOR@@", _swift(str(root / "tool/apple_spm.py"))).encode()
    files["Package.swift"] = _manifest(target, mode, digest)
    selection = {"schema": 1, "generator": "sing_box_flutter.apple_spm", "root": str(root),
                 "project": str(project), "target": target, "python": python, "mode": mode,
                 "installed": [entry["path"], digest],
                 "identity": {key: receipt.get(key) for key in
                              ("schema", "version", "fingerprint", "pubspec_sha256", "macos", "features", "requested_features")},
                 "artifact": entry, "canonical": canonical,
                 "files": {name: _hash(data) for name, data in sorted(files.items())}}
    return selection, files, artifact


def _inventory(package: Path, binary: str) -> dict[str, bytes]:
    files = {}
    for directory, dirs, names in os.walk(package, followlinks=False):
        base = Path(directory)
        if base == package:
            for name in (".build", ".swiftpm"):
                if (base / name).is_symlink():
                    raise BuildError("Unexpected symlink in host-local Swift package")
            dirs[:] = [d for d in dirs if d not in (".build", ".swiftpm")]
        for name in dirs + names:
            path = base / name
            relative = path.relative_to(package).as_posix()
            if path.is_symlink():
                if relative != binary:
                    raise BuildError("Unexpected symlink in host-local Swift package")
                if name in dirs:
                    dirs.remove(name)
            elif path.is_file() and relative != "selection.json":
                files[relative] = path.read_bytes()
    return files


def _check_stage(package: Path, selection: dict, files: dict[str, bytes], artifact: Path) -> None:
    binary = f"Artifacts/{selection['installed'][1]}/Libbox.xcframework"
    link = package / binary
    if not link.is_symlink() or os.readlink(link) != str(artifact):
        raise BuildError("Swift package binary link differs from the selected host artifact")
    if _inventory(package, binary) != files:
        raise BuildError("Staged Swift package sources or manifest changed; rebuild this host's native core")
    if (package / "selection.json").is_symlink() or json.loads((package / "selection.json").read_text()) != selection:
        raise BuildError("Swift package selection changed; rebuild this host's native core")


def prepare(project: Path, receipt: dict, target: str, *, root: Path = ROOT) -> Path:
    """Stage after native.json is published; never write into the canonical plugin."""
    project, root = project.resolve(strict=True), root.resolve(strict=True)
    package = _package(project, target)
    current = _receipt(project)
    if current != receipt:
        raise BuildError("Swift package staging requires the already updated native receipt")
    if sys.version_info < (3, 11):
        raise BuildError("Swift package validation requires Python 3.11 or newer")
    python = str(Path(sys.executable).resolve(strict=True))
    selection, files, artifact = _snapshot(project, current, target, root, python)
    if package.exists():
        marker = package / "selection.json"
        if marker.is_symlink() or not marker.is_file() or json.loads(marker.read_text()).get("generator") != selection["generator"]:
            raise BuildError("Refusing to replace an unrecognized host Swift package")
        previous = json.loads(marker.read_text())
        # Refuse unexpected links before any replacement/deletion.
        _inventory(package, f"Artifacts/{previous['installed'][1]}/Libbox.xcframework")
        try:
            _check_stage(package, selection, files, artifact)
            return package
        except BuildError:
            pass
    package.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=f".{NAME}-", dir=package.parent))
    backup = stage.with_name(stage.name + "-previous")
    published = False
    try:
        for relative, data in files.items():
            path = stage / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        (stage / "selection.json").write_bytes(_json(selection))
        link = stage / f"Artifacts/{selection['installed'][1]}/Libbox.xcframework"
        link.parent.mkdir(parents=True)
        link.symlink_to(artifact, target_is_directory=True)
        _check_stage(stage, selection, files, artifact)
        # Recheck the host pointer before publishing the derivative.
        if _receipt(project) != current:
            raise BuildError("Native selection changed while staging the Swift package")
        _package(project, target)
        if package.exists():
            package.rename(backup)
        try:
            stage.rename(package)
            published = True
        except BaseException:
            if backup.exists():
                backup.rename(package)
            raise
    finally:
        if stage.exists():
            shutil.rmtree(stage)
        if published and backup.exists():
            shutil.rmtree(backup)
    return package


def validate(settings: Path, output: Path) -> None:
    """Read-only guard; the sole write is GeneratedCapabilities.swift in output."""
    from build_core import host_path
    selection = json.loads(settings.read_text(encoding="utf-8"))
    project, root = Path(selection["project"]), Path(selection["root"])
    if project != project.resolve(strict=True) or root != root.resolve(strict=True):
        raise BuildError("Swift package roots changed")
    package = _package(project, selection["target"])
    if settings.absolute() != package / "selection.json" or settings.is_symlink():
        raise BuildError("Swift package settings must belong to this host's generated package")
    current = _receipt(project)
    expected, files, artifact = _snapshot(project, current, selection["target"], root,
                                          selection["python"], tuple(selection["installed"]))
    if expected != selection:
        raise BuildError("Swift package selection or canonical sources changed; rebuild this host's native core")
    _check_stage(package, expected, files, artifact)
    capabilities = host_path(project, expected["artifact"]["capabilities"]).read_bytes()
    encoded = base64.b64encode(capabilities).decode("ascii")
    generated = ('import Foundation\n\ninternal enum SBFSelectedCapabilities {\n'
                 f'    static let data = Data(base64Encoded: "{encoded}")!\n}}\n').encode()
    output.mkdir(parents=True, exist_ok=True)
    destination = output / "GeneratedCapabilities.swift"
    if destination.is_symlink():
        raise BuildError("Generated capabilities output must not be a symlink")
    if not destination.exists() or destination.read_bytes() != generated:
        temporary = None
        try:
            with tempfile.NamedTemporaryFile(dir=output, prefix=".capabilities-", delete=False) as stream:
                temporary = Path(stream.name)
                stream.write(generated)
            temporary.replace(destination)
        finally:
            if temporary is not None:
                temporary.unlink(missing_ok=True)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    guard = commands.add_parser("validate")
    guard.add_argument("--settings", type=Path, required=True)
    guard.add_argument("--output", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        validate(args.settings, args.output)
        return 0
    except (BuildError, OSError, ValueError, KeyError, TypeError, AttributeError):
        print("Selected Apple Swift package is stale or invalid. Run dart run sing_box_flutter build "
              "for this host and target before building the app.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
