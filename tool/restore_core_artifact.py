#!/usr/bin/env python3
"""Restore a compatible selected Android core from a completed CI build."""
from __future__ import annotations

import argparse
from collections.abc import Iterator
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import zipfile

sys.dont_write_bytecode = True
from build_core import ANDROID_TARGETS, RECEIPT, canonical_hash, host_lock, host_path
from core_recipe import verify_current_recipe
from native_common import BuildError, ROOT, legal_files, sha256, write_json

MANIFEST = ".dart_tool/sing_box_flutter/artifact-sha256.json"


class RestoreMiss(BuildError):
    """Missing or incompatible producer output can fall back to a source build."""


def read_object(path: Path) -> dict:
    if path.is_symlink() or not path.is_file():
        raise BuildError("Selected core bundle is incomplete")
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise BuildError("Selected core bundle metadata must be an object")
    return value


def validated_files(directory: Path) -> Iterator[Path]:
    if directory.is_symlink() or not directory.is_dir():
        raise BuildError("Selected core artifact directory is missing")
    for path in sorted(directory.rglob("*")):
        if path.is_symlink() or not (path.is_file() or path.is_dir()):
            raise BuildError("Selected core bundle must contain only regular files and directories")
        if path.is_file():
            yield path


def inventory(directory: Path, project: Path) -> dict[str, str]:
    return {path.relative_to(project).as_posix(): sha256(path) for path in validated_files(directory)}


def bundle(project: Path) -> tuple[dict, dict, Path, dict[str, str]]:
    if project.is_symlink():
        raise BuildError("Selected core project must not be a symlink")
    receipt_path = host_path(project, RECEIPT.as_posix())
    receipt = read_object(receipt_path)
    artifacts = receipt.get("artifacts")
    if not isinstance(artifacts, dict) or set(artifacts) != {"android"}:
        raise BuildError("CI reuse requires exactly one selected Android artifact")
    entry = artifacts["android"]
    fingerprint = receipt.get("fingerprint")
    if (not isinstance(entry, dict) or not isinstance(fingerprint, str) or
            len(fingerprint) != 64 or any(c not in "0123456789abcdef" for c in fingerprint)):
        raise BuildError("Selected core receipt identity is invalid")
    hashes = entry.get("sha256")
    if not isinstance(hashes, dict) or set(hashes) != {"libbox.aar"}:
        raise BuildError("Selected Android artifact hash is missing")
    relative = f".dart_tool/sing_box_flutter/builds/{fingerprint}/android/{canonical_hash(hashes)}"
    for field, name in (("path", "libbox.aar"), ("capabilities", "core-capabilities.json"),
                        ("metadata", "native-build.json")):
        if entry.get(field) != f"{relative}/{name}":
            raise BuildError("Selected core bundle layout does not match its receipt")
    directory = host_path(project, relative)
    files = {RECEIPT.as_posix(): sha256(receipt_path), **inventory(directory, project)}
    metadata = read_object(directory / "native-build.json")
    return receipt, metadata, directory, files


def verify_contents(project: Path, receipt: dict, metadata: dict, directory: Path,
                    root: Path) -> None:
    verify_current_recipe(project, receipt, metadata, target="android", root=root)
    entry = receipt["artifacts"]["android"]
    aar = directory / "libbox.aar"
    if not aar.is_file() or {"libbox.aar": sha256(aar)} != entry["sha256"]:
        raise BuildError("Selected core AAR does not match its receipt")
    if metadata.get("artifact_sha256") != entry["sha256"]:
        raise BuildError("Selected core AAR does not match its build metadata")
    capabilities = directory / "core-capabilities.json"
    asset = directory / "assets/sing_box_flutter/core-capabilities.json"
    for path in (capabilities, asset):
        if not path.is_file() or sha256(path) != entry.get("capabilities_sha256"):
            raise BuildError("Selected core capabilities are missing or changed")
    expected = {key: receipt[key] for key in ("schema", "version", "fingerprint", "features")}
    if read_object(capabilities) != expected:
        raise BuildError("Selected core capabilities do not match the receipt")
    with zipfile.ZipFile(aar) as archive:
        names = archive.namelist()
        if len(names) != len(set(names)) or not {"AndroidManifest.xml", "classes.jar"}.issubset(names):
            raise BuildError("Selected core AAR has an invalid layout")
        required = {f"jni/{abi}/libbox.so" for abi in ANDROID_TARGETS}
        actual = {name for name in names if name.startswith("jni/") and name.endswith("/libbox.so")}
        if actual != required or any(archive.getinfo(name).file_size == 0 for name in required):
            raise BuildError("Selected core AAR must contain all four Android ABIs")
    notices = {**legal_files(root), "NOTICE.mobile-core": root / "native/mobile/NOTICE"}
    for name, current in notices.items():
        included = directory / "Legal" / name
        if not included.is_file() or sha256(included) != sha256(current):
            raise BuildError("Selected core legal notices differ from the current project")
    required_notices = {"LICENSE.sing-box"}
    if "wgc" in receipt["features"]["endpoints"]:
        required_notices.add("LICENSE.wgc")
    for name in required_notices:
        path = directory / "Legal" / name
        if not path.is_file() or path.stat().st_size == 0:
            raise BuildError("Selected core dependency legal notices are missing")
    allowed = {"libbox.aar", "native-build.json", "core-capabilities.json",
               "assets/sing_box_flutter/core-capabilities.json"}
    for path in validated_files(directory):
        relative = path.relative_to(directory).as_posix()
        if relative not in allowed and not relative.startswith("Legal/"):
            raise BuildError("Selected core bundle contains an unexpected file")


def write_manifest(project: Path, *, root: Path = ROOT) -> None:
    with host_lock(project):
        receipt, metadata, directory, files = bundle(project)
        verify_contents(project, receipt, metadata, directory, root)
        write_json(host_path(project, MANIFEST), {"schema": 1, "sha256": files})


def restore_artifact(source: Path, project: Path, *, root: Path = ROOT) -> None:
    # Check the complete producer bundle before touching this host's output.
    try:
        receipt, metadata, directory, files = bundle(source)
        manifest = read_object(host_path(source, MANIFEST))
        if manifest.get("schema") != 1 or manifest.get("sha256") != files:
            raise BuildError("Selected core bundle checksum inventory differs")
        verify_contents(project, receipt, metadata, directory, root)
    except (BuildError, OSError, ValueError, KeyError, TypeError, zipfile.BadZipFile) as error:
        raise RestoreMiss("Selected core bundle is absent or incompatible") from error

    relative = directory.relative_to(source)
    expected = {Path(name).relative_to(relative).as_posix(): digest for name, digest in files.items()
                if name != RECEIPT.as_posix()}
    with host_lock(project):
        destination = host_path(project, relative.as_posix())
        cached = destination.exists()
        if cached and inventory(destination, destination) != expected:
            raise RestoreMiss("Existing selected core output differs from the producer bundle")
        receipt_path = host_path(project, RECEIPT.as_posix())
        destination.parent.mkdir(parents=True, exist_ok=True)
        staging = Path(tempfile.mkdtemp(prefix=".restore-", dir=destination.parent))
        try:
            prepared = staging / "artifact"
            if not cached:
                shutil.copytree(directory, prepared, symlinks=True)
                if inventory(prepared, prepared) != expected:
                    raise RestoreMiss("Selected core bundle changed while copying")
            pending_receipt = staging / "native.json"
            shutil.copyfile(host_path(source, RECEIPT.as_posix()), pending_receipt)
            if (sha256(pending_receipt) != files[RECEIPT.as_posix()] or
                    read_object(pending_receipt) != receipt):
                raise RestoreMiss("Selected core receipt changed while copying")
            # A profile or source change during copying must not publish a pointer.
            try:
                verify_current_recipe(project, receipt, metadata, target="android", root=root)
            except BuildError as error:
                raise RestoreMiss("Selected core recipe changed while copying") from error
            if not cached:
                prepared.rename(destination)
            # Preserve the producer's receipt, including its original pubspec hash.
            os.replace(pending_receipt, receipt_path)
        finally:
            shutil.rmtree(staging)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project-dir", type=Path, required=True)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--from-dir", type=Path, help="Downloaded producer host directory containing .dart_tool")
    mode.add_argument("--write-manifest", action="store_true", help="Record the producer's complete selected output")
    args = parser.parse_args(argv)
    try:
        project = args.project_dir.expanduser().resolve(strict=True)
        if args.write_manifest:
            write_manifest(project)
            print("Recorded selected Android core checksums.")
            return 0
        restored = False
        try:
            restore_artifact(args.from_dir.expanduser().absolute(), project)
            restored = True
            print("Restored the compatible selected Android core; native compilation is unnecessary.")
        except BuildError:
            print("Selected core bundle is absent or incompatible; building from source.")
        if output := os.environ.get("GITHUB_OUTPUT"):
            with open(output, "a") as handle:
                handle.write(f"restored={str(restored).lower()}\n")
        return 0
    except (BuildError, OSError, ValueError, KeyError, TypeError, zipfile.BadZipFile) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
