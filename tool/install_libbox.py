#!/usr/bin/env python3
"""Install this plugin's prebuilt core from an immutable GitHub release tag."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import sys
import tarfile
import tempfile
import urllib.parse
import zipfile

sys.dont_write_bytecode = True
from native_common import (ARTIFACTS, BuildError, RELEASE_REPO, ROOT, archive_name,
                           artifact_hashes, build_directory, download, load_pins,
                           parse_manifest, project_lock, safe_extract, sha256,
                           validate_metadata, write_json)


def install_one(target: str, pins: dict, base: str, sums: dict, cache: Path,
                force: bool, manifest_sha256: str) -> None:
    name = archive_name(pins, target)
    if name not in sums:
        raise BuildError(f"Release SHA256SUMS has no entry for {name}")
    archive, provenance = download(base + "/" + name, cache / "downloads", sums[name])
    staging = Path(tempfile.mkdtemp(prefix=f"install-{target}-", dir=cache))
    try:
        safe_extract(archive, staging)
        relative = ARTIFACTS[target]
        # Do not allow a release to install anything except its declared artifact.
        for path in staging.rglob("*"):
            name_in_archive = path.relative_to(staging).as_posix()
            if (name_in_archive in ("native-build.json", "LICENSE.sing-box") or name_in_archive == relative
                    or name_in_archive.startswith(relative + "/")
                    or (path.is_dir() and relative.startswith(name_in_archive + "/"))):
                continue
            raise BuildError("Release archive contains an unexpected file")
        metadata = json.loads((staging / "native-build.json").read_text())
        validate_metadata(metadata, pins, target)
        license_path = staging / "LICENSE.sing-box"
        if not license_path.is_file() or sha256(license_path) != metadata.get("license_sha256"):
            raise BuildError("Release upstream license is missing or changed")
        artifact = staging / relative
        if artifact_hashes(artifact) != metadata.get("artifact_sha256"):
            raise BuildError("Release artifact does not match its build metadata")
        if target == "android":
            with zipfile.ZipFile(artifact) as aar:
                if not {"AndroidManifest.xml", "classes.jar"}.issubset(aar.namelist()):
                    raise BuildError("Release AAR has no manifest/classes.jar")
        elif not (artifact / "Info.plist").is_file():
            raise BuildError("Release XCFramework has no Info.plist")
        destination = ROOT / relative
        if destination.is_symlink() or not destination.resolve().is_relative_to(ROOT.resolve()):
            raise BuildError("Artifact destination must stay inside this plugin")
        if destination.exists() and artifact_hashes(destination) == metadata["artifact_sha256"]:
            print(f"{target}: matching core already installed.")
        else:
            if destination.exists() and not force:
                raise BuildError("Different core is already installed; use --force to replace after validation")
            destination.parent.mkdir(parents=True, exist_ok=True)
            backup = staging / "previous-artifact"
            if destination.exists():
                destination.rename(backup)
            try:
                artifact.rename(destination)
            except OSError:
                if backup.exists():
                    backup.rename(destination)
                raise
            print(f"{target}: installed {relative}")
        write_json(cache / "installed" / f"{target}.json", {
            "schema": 1, "release_url": base, "manifest_sha256": manifest_sha256,
            "download": provenance, "build": metadata,
        })
    finally:
        shutil.rmtree(staging)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", choices=("android", "apple", "all"), default="all")
    parser.add_argument("--release", required=True, help="Published tag, e.g. v0.1.0; never latest")
    parser.add_argument("--manifest-sha256", help="Optional independently obtained SHA-256 of SHA256SUMS")
    parser.add_argument("--cache-dir", default=str(ROOT / ".build/install"))
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--force", action="store_true", help="Replace an existing core after all validation")
    args = parser.parse_args(argv)
    try:
        pins = load_pins()
        cache = build_directory(args.cache_dir)
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", args.release) or args.release == "latest":
            raise BuildError("Use an explicit release tag without paths")
        if args.manifest_sha256 and not re.fullmatch(r"[0-9a-f]{64}", args.manifest_sha256):
            raise BuildError("--manifest-sha256 must be a lowercase SHA-256 digest")
        targets = ["android", "apple"] if args.target == "all" else [args.target]
        base = f"https://github.com/{RELEASE_REPO}/releases/download/{urllib.parse.quote(args.release, safe='')}"
        if args.dry_run:
            print(json.dumps({"manifest": base + "/SHA256SUMS", "archives": [base + "/" + archive_name(pins, t) for t in targets],
                              "validation": ["HTTPS release", "SHA256SUMS", "safe extraction", "core/toolchain pins", "artifact hashes"],
                              "outputs": [ARTIFACTS[t] for t in targets]}, indent=2))
            return 0
        with project_lock():
            manifest, provenance = download(base + "/SHA256SUMS", cache / "downloads",
                                             args.manifest_sha256, max_bytes=1024 * 1024)
            sums = parse_manifest(manifest.read_text())
            for target in targets:
                install_one(target, pins, base, sums, cache, args.force, provenance["sha256"])
        return 0
    except (BuildError, OSError, ValueError, KeyError, tarfile.TarError, zipfile.BadZipFile) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
