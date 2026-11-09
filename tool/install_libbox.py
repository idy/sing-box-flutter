#!/usr/bin/env python3
"""Install this plugin's prebuilt core from an immutable GitHub release tag."""
from __future__ import annotations

import argparse
import hashlib
import json
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
                           artifact_hashes, build_directory, download, legal_files, load_pins,
                           parse_manifest, project_lock, safe_extract, sha256,
                           validate_metadata, write_json)


def install_one(target: str, pins: dict, source: str | Path, sums: dict, cache: Path,
                force: bool, manifest_sha256: str) -> None:
    name = archive_name(pins, target)
    if name not in sums:
        raise BuildError(f"Release SHA256SUMS has no entry for {name}")
    if isinstance(source, Path):
        original = source / name
        if original.is_symlink() or not original.is_file():
            raise BuildError("Local core archive must be a regular file")
        # Snapshot local/CI input into an isolated private directory, then verify.
        # Extraction never follows a file that the source directory can replace.
        isolated = Path(tempfile.mkdtemp(prefix="local-archive-", dir=cache))
        archive = isolated / "payload"
        shutil.copyfile(original, archive)
        if sha256(archive) != sums[name]:
            raise BuildError("Local archive SHA-256 does not match SHA256SUMS")
        provenance = {"source": "local-release-directory", "name": name, "sha256": sums[name],
                      "size": archive.stat().st_size}
        release_source = "local-release-directory"
    else:
        archive, provenance = download(source + "/" + name, cache / "downloads", sums[name])
        release_source = source
    staging = Path(tempfile.mkdtemp(prefix=f"install-{target}-", dir=cache))
    keep_staging, committed = False, False
    try:
        safe_extract(archive, staging)
        legal = staging / "Legal"
        if not legal.is_dir():
            raise BuildError("Release legal notices must be a directory")
        if legal.is_symlink() or any(path.is_symlink() for path in legal.rglob("*")):
            raise BuildError("Release legal notices must not contain symlinks")
        relative = ARTIFACTS[target]
        # Do not allow a release to install anything except its declared artifact.
        for path in staging.rglob("*"):
            name_in_archive = path.relative_to(staging).as_posix()
            if (name_in_archive in ("native-build.json", "LICENSE.sing-box", "Legal") or name_in_archive.startswith("Legal/") or name_in_archive == relative
                    or name_in_archive.startswith(relative + "/")
                    or (path.is_dir() and relative.startswith(name_in_archive + "/"))):
                continue
            raise BuildError("Release archive contains an unexpected file")
        metadata = json.loads((staging / "native-build.json").read_text())
        validate_metadata(metadata, pins, target, allow_custom=isinstance(source, Path))
        license_path = staging / "LICENSE.sing-box"
        if not license_path.is_file() or sha256(license_path) != metadata.get("license_sha256"):
            raise BuildError("Release upstream license is missing or changed")
        required_legal = {*legal_files(), "LICENSE.wgc"}
        missing_legal = sorted(name for name in required_legal if not (legal / name).is_file())
        if missing_legal:
            raise BuildError("Release required license notices are missing: " + ", ".join(missing_legal))
        if artifact_hashes(staging / "Legal") != metadata.get("legal_sha256"):
            raise BuildError("Release WGC/dependency license notices are missing or changed")
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
        legal_path = ROOT / ".build/legal" / target
        if any(path.is_symlink() for path in (ROOT / ".build", legal_path.parent, legal_path)):
            raise BuildError("Legal notice destination must be inside project .build without symlinks")
        legal_destination = build_directory(legal_path, ROOT)
        if cache.resolve().is_relative_to(legal_destination):
            raise BuildError("Install cache must be separate from the legal notice destination")
        if legal_destination.exists() and not legal_destination.is_dir():
            raise BuildError("Legal notice destination must be a directory")
        receipt_destination = cache / "installed" / f"{target}.json"
        if any(path.is_symlink() for path in (cache, receipt_destination.parent, receipt_destination)):
            raise BuildError("Installation metadata destination must not contain symlinks")
        if receipt_destination.exists() and not receipt_destination.is_file():
            raise BuildError("Installation metadata destination must be a regular file")
        matching = destination.exists() and artifact_hashes(destination) == metadata["artifact_sha256"]
        if destination.exists() and not matching and not force:
            raise BuildError("Different core is already installed; use --force to replace after validation")
        for parent in (destination.parent, legal_destination.parent, receipt_destination.parent):
            parent.mkdir(parents=True, exist_ok=True)
        # Prepare all copied/written output before changing installed files.
        next_legal, next_receipt = staging / "next-legal", staging / "next-install.json"
        shutil.copytree(legal, next_legal)
        write_json(next_receipt, {
            "schema": 1, "release_source": release_source, "manifest_sha256": manifest_sha256,
            "download": provenance, "build": metadata,
        })
        replacements = [] if matching else [(artifact, destination)]
        replacements += [(next_legal, legal_destination), (next_receipt, receipt_destination)]
        changed = []
        try:
            for index, (prepared, installed) in enumerate(replacements):
                backup = staging / f"previous-{index}" if installed.exists() else None
                if backup is not None:
                    installed.rename(backup)
                changed.append((prepared, installed, backup))
                prepared.rename(installed)
        except BaseException:
            try:
                for prepared, installed, backup in reversed(changed):
                    if installed.exists():
                        installed.rename(prepared)
                    if backup is not None:
                        backup.rename(installed)
            except BaseException as rollback_error:
                keep_staging = True
                raise BuildError(f"Install rollback failed; backups retained at {staging}") from rollback_error
            raise
        committed = True
        print(f"{target}: matching core already installed." if matching else f"{target}: installed {relative}")
    finally:
        if not keep_staging:
            try:
                shutil.rmtree(staging)
            except OSError:
                if not committed:
                    raise
                print(f"warning: Installation completed; staging cleanup failed at {staging}", file=sys.stderr)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", choices=("android", "apple", "all"), default="all")
    source_args = parser.add_mutually_exclusive_group(required=True)
    source_args.add_argument("--release", help="Published tag, e.g. v0.2.0; never latest")
    source_args.add_argument("--from-dir", type=Path, help="Install verified local/CI release inputs without networking")
    parser.add_argument("--manifest-sha256", help="Optional independently obtained SHA-256 of SHA256SUMS")
    parser.add_argument("--cache-dir", default=str(ROOT / ".build/install"))
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--force", action="store_true", help="Replace an existing core after all validation")
    args = parser.parse_args(argv)
    try:
        pins = load_pins()
        cache = build_directory(args.cache_dir)
        if args.release and (not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", args.release) or args.release == "latest"):
            raise BuildError("Use an explicit release tag without paths")
        if args.manifest_sha256 and not re.fullmatch(r"[0-9a-f]{64}", args.manifest_sha256):
            raise BuildError("--manifest-sha256 must be a lowercase SHA-256 digest")
        targets = ["android", "apple"] if args.target == "all" else [args.target]
        source = args.from_dir.expanduser().resolve() if args.from_dir else f"https://github.com/{RELEASE_REPO}/releases/download/{urllib.parse.quote(args.release, safe='')}"
        if args.dry_run:
            prefix = str(source)
            print(json.dumps({"manifest": prefix + "/SHA256SUMS", "archives": [prefix + "/" + archive_name(pins, t) for t in targets],
                              "validation": ["SHA256SUMS", "safe extraction", "core/toolchain pins", "mobile profile", "artifact hashes"],
                              "outputs": [ARTIFACTS[t] for t in targets]}, indent=2))
            return 0
        with project_lock():
            cache.mkdir(parents=True, exist_ok=True)
            if isinstance(source, Path):
                manifest = source / "SHA256SUMS"
                if manifest.is_symlink() or not manifest.is_file() or manifest.stat().st_size > 1024 * 1024:
                    raise BuildError("Local SHA256SUMS must be a regular file under 1 MiB")
                manifest_bytes = manifest.read_bytes()
                digest = hashlib.sha256(manifest_bytes).hexdigest()
                if args.manifest_sha256 and digest != args.manifest_sha256:
                    raise BuildError("Local SHA256SUMS does not match --manifest-sha256")
            else:
                manifest, provenance = download(source + "/SHA256SUMS", cache / "downloads",
                                                 args.manifest_sha256, max_bytes=1024 * 1024)
                manifest_bytes = manifest.read_bytes()
                digest = provenance["sha256"]
            sums = parse_manifest(manifest_bytes.decode())
            for target in targets:
                install_one(target, pins, source, sums, cache, args.force, digest)
        return 0
    except (BuildError, OSError, ValueError, KeyError, tarfile.TarError, zipfile.BadZipFile) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
