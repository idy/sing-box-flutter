#!/usr/bin/env python3
"""Read-only validation of a selected artifact against the resolved plugin recipe."""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys

sys.dont_write_bytecode = True
# -I deliberately removes the script directory. Restore only this trusted package
# directory, never the host project or downloaded artifact, for sibling imports.
sys.path.insert(0, str(Path(__file__).resolve().parent))
from build_core import build_identity, canonical_hash, host_path, receipt_for_target
from core_features import canonicalize, resolve_features
from native_common import BuildError, ROOT, artifact_hashes, load_pins, sha256
from windows_core import WINDOWS_ARCHES, validate_windows_bundle


def verify_current_recipe(project: Path, receipt: dict, metadata: dict, *,
                          target: str, root: Path = ROOT) -> None:
    """Validate recipe inputs only; callers separately validate artifact contents.

    Extension descriptors remain locked to their recorded sanitized identities.
    This checks the current plugin source/pins/tooling, without resolving private
    extension paths, compiling Go, accessing credentials, or downloading anything.
    """
    try:
        project = project.resolve(strict=True)
        root = root.resolve(strict=True)
        pins = load_pins(root)
        if (not isinstance(receipt, dict) or not isinstance(metadata, dict) or
                receipt.get("schema") != 1 or metadata.get("schema") != 1 or
                metadata.get("mode") != "selectable-core" or metadata.get("target") != target or
                receipt.get("version") != pins["sing_box_version"] or
                receipt.get("pubspec_sha256") != sha256(project / "pubspec.yaml")):
            raise ValueError("invalid receipt")
        requested = canonicalize(receipt["requested_features"])
        identity = metadata["identity"]
        if not isinstance(identity, dict):
            raise ValueError("invalid identity")
        extensions = identity.get("extensions")
        if (not isinstance(extensions, list) or
                any(not isinstance(entry, dict) or "path" in entry for entry in extensions)):
            raise ValueError("invalid extension descriptors")
        windows_arch = identity.get("windows_arch", "amd64")
        if target == "windows" and (windows_arch not in WINDOWS_ARCHES or
                receipt.get("windows_arch") != windows_arch or metadata.get("windows_arch") != windows_arch):
            raise ValueError("Windows architecture differs from the recipe")
        expected = build_identity(requested, extensions, pins, root, target=target, windows_arch=windows_arch)
        fingerprint = canonical_hash(expected)
        resolved = resolve_features(requested, target)
        if (identity != expected or receipt.get("fingerprint") != fingerprint or
                metadata.get("fingerprint") != fingerprint or
                receipt.get("features") != resolved["features"] or
                metadata.get("features") != resolved["features"] or
                metadata.get("tags") != resolved["tags"]):
            raise ValueError("stale recipe")
    except (BuildError, OSError, ValueError, KeyError, TypeError) as error:
        raise BuildError("Selected native core recipe is stale or invalid. "
                         "Run dart run sing_box_flutter build for this host and target.") from error


def verify_current_artifact(project: Path, receipt: dict, target: str,
                            installed: tuple[str, str] | None = None) -> str:
    """Recheck the artifact a native build is about to link against its receipt.

    `installed` is the (path, digest) CocoaPods baked in at pod install. Xcode
    links that framework, so a receipt naming any other artifact needs a new
    pod install even when the new artifact is itself valid. Returns the digest.
    """
    try:
        entry = receipt["artifacts"][target]
        expected = entry["sha256"]
        artifact = host_path(project, entry["path"])
        capabilities = host_path(project, entry["capabilities"])
        if (not isinstance(expected, dict) or not expected or artifact_hashes(artifact) != expected or
                sha256(capabilities) != entry["capabilities_sha256"]):
            raise ValueError("modified artifact")
        manifest = json.loads(capabilities.read_text(encoding="utf-8"))
        if any(manifest.get(key) != receipt.get(key) for key in ("schema", "version", "fingerprint", "features")):
            raise ValueError("capabilities disagree with receipt")
        if target == "windows":
            if manifest.get("windows_arch") != receipt.get("windows_arch"):
                raise ValueError("capabilities architecture disagrees with receipt")
            validate_windows_bundle(artifact, receipt["windows_arch"])
        digest = canonical_hash(expected)
    except (BuildError, OSError, ValueError, KeyError, TypeError, AttributeError) as error:
        raise BuildError("Selected native core artifact changed after it was built. "
                         "Run dart run sing_box_flutter build for this host and target.") from error
    if installed is not None:
        selected = entry["path"]
        if target == "windows" and Path(installed[0]).is_absolute():
            selected = str(artifact.resolve())
            installed = (str(Path(installed[0]).resolve()), installed[1])
        if (selected, digest) != installed:
            if target == "windows":
                raise BuildError("The selected Windows core changed since CMake configuration. "
                                 "Reconfigure CMake (or run flutter build windows) before building the app.")
            raise BuildError("The selected native core changed since pod install. "
                             "Run pod install (or flutter build) before building the app.")
    return digest


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project-dir", type=Path, required=True)
    parser.add_argument("--target", choices=("android", "apple", "windows"), required=True)
    parser.add_argument("--windows-arch", choices=tuple(WINDOWS_ARCHES),
                        help="Require this Windows bundle architecture")
    parser.add_argument("--installed-artifact", help="Artifact path baked in at pod install or CMake configuration")
    parser.add_argument("--installed-sha256", help="Artifact digest baked in at pod install or CMake configuration")
    output = parser.add_mutually_exclusive_group()
    output.add_argument("--print-artifact-digest", action="store_true",
                        help="Print only the verified artifact digest")
    output.add_argument("--print-artifact-path", action="store_true",
                        help="Print only the validated absolute artifact path (WindowsCore directory on Windows)")
    output.add_argument("--print-artifact-pair", action="store_true",
                        help="Print the verified digest and absolute path as one newline-delimited pair")
    args = parser.parse_args(argv)
    if (args.installed_artifact is None) != (args.installed_sha256 is None):
        parser.error("--installed-artifact and --installed-sha256 must be used together")
    installed = None if args.installed_artifact is None else (args.installed_artifact, args.installed_sha256)
    try:
        project = args.project_dir.expanduser().resolve(strict=True)
        receipt_path = host_path(project, receipt_for_target(args.target).as_posix())
        receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
        entry = receipt["artifacts"][args.target]
        metadata_path = host_path(project, entry["metadata"])
        metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
        verify_current_recipe(project, receipt, metadata, target=args.target)
        if args.windows_arch is not None and (args.target != "windows" or
                metadata["identity"].get("windows_arch") != args.windows_arch):
            raise BuildError("The selected Windows core architecture differs from this app target. "
                             "Rebuild with --target windows --windows-arch for the app architecture.")
        # Build-time guards run after pod install/Gradle configuration; recheck the
        # bytes that will be linked, not only the recipe that produced them.
        digest = verify_current_artifact(project, receipt, args.target, installed)
        if args.print_artifact_pair:
            # One validated snapshot; scalar consumers split only the first newline
            # so spaces, semicolons and backslashes in the path remain literal.
            artifact = host_path(project, entry["path"]).resolve(strict=True)
            print(f"{digest}\n{artifact}")
        elif args.print_artifact_digest:
            print(digest)
        elif args.print_artifact_path:
            print(host_path(project, entry["path"]).resolve(strict=True))
        return 0
    except BuildError as error:
        # BuildError messages are fixed text without paths, configs or credentials.
        print(str(error), file=sys.stderr)
        return 1
    except (OSError, ValueError, KeyError, TypeError):
        print("Selected native core recipe is stale or invalid. "
              "Rebuild this host's native core before building the app.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
