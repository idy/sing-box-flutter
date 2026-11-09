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
from build_core import RECEIPT, build_identity, canonical_hash, host_path
from core_features import canonicalize, resolve_features
from native_common import BuildError, ROOT, artifact_hashes, load_pins, sha256


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
        expected = build_identity(requested, extensions, pins, root)
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


def verify_current_artifact(project: Path, receipt: dict, target: str) -> None:
    """Recheck the artifact a native build is about to link against its receipt."""
    try:
        entry = receipt["artifacts"][target]
        expected = entry["sha256"]
        artifact = host_path(project, entry["path"])
        capabilities = host_path(project, entry["capabilities"])
        if (not isinstance(expected, dict) or not expected or artifact_hashes(artifact) != expected or
                sha256(capabilities) != entry["capabilities_sha256"]):
            raise ValueError("modified artifact")
        manifest = json.loads(capabilities.read_text())
        if any(manifest.get(key) != receipt.get(key) for key in ("schema", "version", "fingerprint", "features")):
            raise ValueError("capabilities disagree with receipt")
    except (BuildError, OSError, ValueError, KeyError, TypeError, AttributeError) as error:
        raise BuildError("Selected native core artifact changed after it was built. "
                         "Run dart run sing_box_flutter build for this host and target.") from error


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project-dir", type=Path, required=True)
    parser.add_argument("--target", choices=("android", "apple"), required=True)
    args = parser.parse_args(argv)
    try:
        project = args.project_dir.expanduser().resolve(strict=True)
        receipt_path = host_path(project, RECEIPT.as_posix())
        receipt = json.loads(receipt_path.read_text())
        entry = receipt["artifacts"][args.target]
        metadata_path = host_path(project, entry["metadata"])
        metadata = json.loads(metadata_path.read_text())
        verify_current_recipe(project, receipt, metadata, target=args.target)
        # Build-time guards run after pod install/Gradle configuration; recheck the
        # bytes that will be linked, not only the recipe that produced them.
        verify_current_artifact(project, receipt, args.target)
        return 0
    except (BuildError, OSError, ValueError, KeyError, TypeError):
        print("Selected native core recipe is stale or invalid. "
              "Rebuild this host's native core before building the app.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
