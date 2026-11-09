#!/usr/bin/env python3
"""Build a host-selected mobile core without installing into the shared pub cache."""
from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
from build_libbox import (bootstrap_go, check_sdks, install_mobile, resolved_tailscale_dependency,
                          retain_wgc_license, run, source_checkout)
from core_features import _unique_object, canonicalize, generate_registry, resolve_features
from native_common import (BuildError, ROOT, artifact_hashes, build_directory, legal_files,
                           load_pins, pin_identity, project_lock, sha256, write_json)
from wgc_overlay import load_extensions, wgc_overlay

ANDROID_TARGETS = {"armeabi-v7a": "android/arm", "arm64-v8a": "android/arm64",
                   "x86": "android/386", "x86_64": "android/amd64"}
RECEIPT = Path(".dart_tool/sing_box_flutter/native.json")


def canonical_hash(value: dict) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def mobile_sources(root: Path = ROOT) -> dict[str, str]:
    directory = root / "native/mobile"
    if not directory.is_dir() or directory.is_symlink():
        raise BuildError("The selectable mobile core source is missing")
    result = {}
    for path in sorted(directory.rglob("*")):
        if path.is_symlink():
            raise BuildError("Selectable core source must not contain symlinks")
        if path.is_file() and not any(part.startswith(".") for part in path.relative_to(directory).parts):
            result[path.relative_to(directory).as_posix()] = sha256(path)
    if not any(name.endswith(".go") for name in result):
        raise BuildError("The selectable mobile core contains no Go source")
    return result


def build_identity(selection: dict, extensions: list[dict], pins: dict, root: Path = ROOT) -> dict:
    return {"schema": 1, "pins": pin_identity(pins, root), "selection": selection,
            "mobile_sources": mobile_sources(root),
            "tooling": {name: sha256(root / "tool" / name) for name in
                        ("build_core.py", "core_features.py", "build_libbox.py", "native_common.py",
                         "wgc_overlay.py", "source_inputs.py")},
            "extensions": [{key: value for key, value in entry.items() if key != "path"}
                           for entry in extensions]}


def load_build_request(path: Path, project: Path) -> tuple[dict, str]:
    """Read the immutable Dart handoff, bound to exactly its parsed host bytes."""
    try:
        request = json.loads(path.read_text(), object_pairs_hook=_unique_object)
        if (not isinstance(request, dict) or set(request) != {"schema", "pubspec", "features"} or
                type(request["schema"]) is not int or request["schema"] != 1):
            raise ValueError("invalid request envelope")
        source = request["pubspec"]
        if not isinstance(source, dict) or set(source) != {"path", "sha256"}:
            raise ValueError("invalid pubspec snapshot")
        if not isinstance(source["path"], str) or not Path(source["path"]).is_absolute():
            raise ValueError("pubspec path must be absolute")
        actual = Path(source["path"]).resolve(strict=True)
        expected = (project / "pubspec.yaml").resolve(strict=True)
        digest = source["sha256"]
        if (actual != expected or actual.name != "pubspec.yaml" or
                not isinstance(digest, str) or len(digest) != 64 or
                any(character not in "0123456789abcdef" for character in digest) or
                sha256(actual) != digest):
            raise ValueError("pubspec snapshot does not match the host")
        return canonicalize(request["features"]), digest
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise BuildError("Invalid or changed pubspec snapshot. "
                         "Run dart run sing_box_flutter build again from the host project.") from error


def host_path(project: Path, relative: str) -> Path:
    """Generated receipts may refer only to real paths inside this host's cache."""
    if not isinstance(relative, str) or not relative or Path(relative).is_absolute():
        raise BuildError("Native artifact paths must be relative to the Flutter project")
    raw = project / relative
    boundary = project / ".dart_tool/sing_box_flutter"
    if ".." in Path(relative).parts or not raw.is_relative_to(boundary):
        raise BuildError("Native artifact path escapes the host project cache")
    for path in (raw, *raw.parents):
        if path == project:
            break
        if path.is_symlink():
            raise BuildError("Native artifact paths must not traverse symlinks")
    if not raw.resolve().is_relative_to(project.resolve()):
        raise BuildError("Native artifact path escapes the host project")
    return raw


@contextlib.contextmanager
def host_lock(project: Path):
    import fcntl
    path = host_path(project, ".dart_tool/sing_box_flutter/build.lock")
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise BuildError("Another native build is using this Flutter host project") from error
        yield


@contextlib.contextmanager
def mobile_overlay(source: Path, registry: str, expected_sources: dict[str, str], root: Path = ROOT):
    """Replace only libbox's binding package, restoring official inputs on all exits."""
    if mobile_sources(root) != expected_sources:
        raise BuildError("Mobile core source changed after the build identity was captured")
    original = source / "experimental/libbox"
    # Keep the backup outside the upstream checkout: go mod tidy scans packages.
    backup_root = Path(tempfile.mkdtemp(prefix="mobile-backup-", dir=source.parent))
    backup = backup_root / "libbox"
    moved = False
    try:
        original.rename(backup)
        moved = True
        original.mkdir()
        for name in expected_sources:
            if name in ("go.mod", "go.sum"):
                continue
            path = root / "native/mobile" / name
            destination = original / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, destination)
            if sha256(destination) != expected_sources[name]:
                raise BuildError("Mobile core source changed while capturing the build")
        (original / "registry_selected.go").write_text(registry)
        yield
    finally:
        if moved:
            if original.exists():
                shutil.rmtree(original)
            backup.rename(original)
        shutil.rmtree(backup_root)


def bind_command(env: dict, output: Path, target: str, tags: list[str],
                 fingerprint: str, pins: dict, android_abis: list[str], apple_platform: str) -> list[str | Path]:
    linker = ("-s -w -buildid= -checklinkname=0 "
              f"-X github.com/sagernet/sing-box/constant.Version={pins['sing_box_version']} "
              f"-X github.com/sagernet/sing-box/experimental/libbox.coreFingerprint={fingerprint}")
    command: list[str | Path] = [Path(env["GOBIN"]) / "gomobile", "bind", "-v", "-o", output]
    if target == "android":
        command += ["-target", ",".join(ANDROID_TARGETS[name] for name in android_abis),
                    "-androidapi", "24", "-javapkg=io.nekohasekai", "-libname=box"]
    else:
        command += ["-target", apple_platform, "-libname=box", "-iosversion=15.0",
                    "-tags-not-macos=with_low_memory"]
    command += ["-trimpath", "-buildvcs=false", "-ldflags", linker,
                "-tags", ",".join(sorted(set(tags) | {"badlinkname", "tfogo_checklinkname0"})),
                "./experimental/libbox"]
    return command


def validate_artifact(path: Path, target: str) -> None:
    if target == "android":
        import zipfile
        try:
            with zipfile.ZipFile(path) as archive:
                names = set(archive.namelist())
                if not {"classes.jar", "AndroidManifest.xml"}.issubset(names):
                    raise BuildError("The selected core AAR is incomplete")
                if not any(name.startswith("jni/") and name.endswith("/libbox.so") for name in names):
                    raise BuildError("The selected core AAR has no native library")
        except (OSError, zipfile.BadZipFile) as error:
            raise BuildError("The selected core did not produce a valid AAR") from error
    elif not (path / "Info.plist").is_file():
        raise BuildError("The selected core did not produce a valid XCFramework")


def publish_artifact(project: Path, output: Path, target: str, capabilities: dict,
                     metadata: dict, notices: dict[str, Path]) -> dict:
    """Publish an immutable content-addressed artifact before updating the pointer."""
    validate_artifact(output, target)
    hashes = artifact_hashes(output)
    metadata = {**metadata, "artifact_sha256": hashes}
    content = canonical_hash(hashes)
    relative = Path(".dart_tool/sing_box_flutter/builds") / capabilities["fingerprint"] / target / content
    final = host_path(project, relative.as_posix())
    final.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=".stage-", dir=final.parent))
    name = "libbox.aar" if target == "android" else "Libbox.xcframework"
    try:
        # Validate small provenance/notice inputs even on a cache hit. Keep the
        # first immutable provenance, but do not copy a whole duplicate SDK.
        write_json(stage / "core-capabilities.json", capabilities)
        write_json(stage / "native-build.json", metadata)
        for filename, path in notices.items():
            destination = stage / "Legal" / filename
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, destination)
        if final.exists():
            if artifact_hashes(final / name) != hashes or json.loads((final / "core-capabilities.json").read_text()) != capabilities:
                raise BuildError("An existing content-addressed artifact was modified")
            if target == "android":
                asset = host_path(project, (relative / "assets/sing_box_flutter/core-capabilities.json").as_posix())
                if not asset.is_file() or sha256(asset) != sha256(final / "core-capabilities.json"):
                    raise BuildError("An existing Android capabilities asset is missing or changed; remove that cached artifact and rebuild")
        else:
            if output.is_dir():
                shutil.copytree(output, stage / name, symlinks=True)
            else:
                shutil.copyfile(output, stage / name)
            if artifact_hashes(stage / name) != hashes:
                raise BuildError("Native artifact changed while copying it to the Flutter project")
            if target == "android":
                write_json(stage / "assets/sing_box_flutter/core-capabilities.json", capabilities)
            stage.rename(final)
    finally:
        if stage.exists():
            shutil.rmtree(stage)
    return {"path": (relative / name).as_posix(),
            "capabilities": (relative / "core-capabilities.json").as_posix(),
            "metadata": (relative / "native-build.json").as_posix(),
            "sha256": hashes, "capabilities_sha256": sha256(final / "core-capabilities.json")}


def update_receipt(project: Path, capabilities: dict, requested: dict, artifacts: dict,
                   expected_pubspec_sha256: str | None = None) -> None:
    current_pubspec = sha256(project / "pubspec.yaml")
    if expected_pubspec_sha256 is not None and current_pubspec != expected_pubspec_sha256:
        raise BuildError("Host pubspec changed during the native build; select the profile and rebuild")
    path = host_path(project, RECEIPT.as_posix())
    previous = json.loads(path.read_text()) if path.exists() else {}
    retained = previous.get("artifacts", {}) if previous.get("fingerprint") == capabilities["fingerprint"] else {}
    if not isinstance(retained, dict):
        raise BuildError("Malformed existing native core receipt")
    for target, entry in retained.items():
        if target in artifacts:
            continue
        if (artifact_hashes(host_path(project, entry["path"])) != entry["sha256"] or
                sha256(host_path(project, entry["capabilities"])) != entry["capabilities_sha256"]):
            raise BuildError("A previously built platform artifact was modified; rebuild that platform")
    write_json(path, {**capabilities, "requested_features": requested,
                      "pubspec_sha256": current_pubspec,
                      "artifacts": {**retained, **artifacts}})


def invalidate_pods(project: Path) -> None:
    """Make Flutter refresh dynamic podspec paths without unlocking pod versions."""
    manifest = project / "ios/Pods/Manifest.lock"
    if not manifest.exists() and not manifest.is_symlink():
        return
    if (manifest.is_symlink() or not manifest.is_file() or
            not manifest.resolve().is_relative_to(project.resolve())):
        raise BuildError("Cannot refresh CocoaPods metadata outside this host; run pod install manually")
    # This generated sandbox receipt is disposable. Keep ios/Podfile.lock intact;
    # Flutter detects its missing counterpart and runs pod install on next build.
    manifest.read_bytes()
    manifest.unlink()


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--features-file", type=Path, required=True)
    result.add_argument("--project-dir", type=Path, required=True)
    result.add_argument("--target", choices=("apple", "android", "all"), default="all")
    result.add_argument("--android-abis", default=",".join(ANDROID_TARGETS))
    result.add_argument("--apple-platform", choices=("ios", "ios,iossimulator"), default="ios,iossimulator")
    result.add_argument("--wgc-config", type=Path)
    result.add_argument("--bootstrap-dir", default=str(ROOT / ".build/bootstrap"))
    result.add_argument("--cache-dir", default=str(ROOT / ".build/cache"))
    result.add_argument("--dry-run", action="store_true")
    result.add_argument("--bootstrap-only", action="store_true")
    return result


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        project = args.project_dir.expanduser().resolve(strict=True)
        if not (project / "pubspec.yaml").is_file():
            raise BuildError("--project-dir must name the Flutter host project containing pubspec.yaml")
        selection, pubspec_sha256 = load_build_request(args.features_file, project)
        if "tun" not in selection["inbounds"]:
            raise BuildError("A mobile VPN core must include the tun inbound")
        pins = load_pins()
        extensions = load_extensions(args.wgc_config)
        if extensions and "wgc" not in selection.get("endpoints", []):
            raise BuildError("Compiled WGC transports require the wgc endpoint to be selected")
        android_abis = args.android_abis.split(",")
        if (not android_abis or len(android_abis) != len(set(android_abis)) or
                any(name not in ANDROID_TARGETS for name in android_abis)):
            raise BuildError("--android-abis must list unique supported Android ABI names")
        bootstrap = build_directory(args.bootstrap_dir)
        cache = build_directory(args.cache_dir)
        if bootstrap == cache or bootstrap.is_relative_to(cache) or cache.is_relative_to(bootstrap):
            raise BuildError("Bootstrap and cache directories must be separate, non-nested .build directories")
        targets = ["android", "apple"] if args.target == "all" else [args.target]
        resolved = {target: resolve_features(selection, target) for target in targets}
        identity = build_identity(selection, extensions, pins)
        fingerprint = canonical_hash(identity)
        registry = generate_registry(next(iter(resolved.values()))["features"], extensions)
        if args.dry_run:
            print(json.dumps({"mode": "selectable-core", "version": pins["sing_box_version"],
                              "fingerprint": fingerprint, "selection": selection, "resolved": resolved,
                              "registry_sha256": hashlib.sha256(registry.encode()).hexdigest(),
                              "receipt": RECEIPT.as_posix(), "android_abis": android_abis,
                              "apple_platform": args.apple_platform, "shared_pub_cache_artifacts": False}, indent=2))
            return 0
        # The package lock protects its verified toolchain/source; the host lock
        # also protects per-project receipts across different pub-cache versions.
        with contextlib.ExitStack() as locks:
            locks.enter_context(project_lock())
            locks.enter_context(host_lock(project))
            cache.mkdir(parents=True, exist_ok=True)
            bootstrap.mkdir(parents=True, exist_ok=True)
            sdk_env = dict(os.environ)
            sdk_versions = {} if args.bootstrap_only else check_sdks(targets, sdk_env)
            go, env, toolchain = bootstrap_go(pins, bootstrap, cache)
            for key in ("ANDROID_HOME", "ANDROID_SDK_ROOT", "ANDROID_NDK_HOME", "NDK"):
                if key in sdk_env:
                    env[key] = sdk_env[key]
            source = source_checkout(pins, bootstrap)
            mobile, tailscale = install_mobile(go, env, source, pins)
            if args.bootstrap_only:
                print("Verified selectable-core compiler and mobile tools are ready.")
                return 0
            artifacts = {}
            for target in targets:
                effective = resolved[target]
                capabilities = {"schema": 1, "version": pins["sing_box_version"],
                                "fingerprint": fingerprint, "features": effective["features"]}
                stage = Path(tempfile.mkdtemp(prefix=f"selectable-{target}-", dir=cache))
                output = stage / ("libbox.aar" if target == "android" else "Libbox.xcframework")
                try:
                    selected_registry = generate_registry(effective["features"], extensions)
                    with mobile_overlay(source, selected_registry, identity["mobile_sources"]):
                        with contextlib.ExitStack() as stack:
                            wgc = None
                            wgc_license = None
                            if "wgc" in effective["features"]["endpoints"]:
                                wgc = stack.enter_context(wgc_overlay(source, go, env, run, extensions))
                                wgc_license = retain_wgc_license(go, env, source, cache, pins)
                            if any("tailscale" in values for values in effective["features"].values()):
                                tailscale = resolved_tailscale_dependency(go, env, source, pins)
                            # Exercise the selected constructor graph and lifecycle on
                            # the build host before cross-compiling native bindings.
                            run([go, "test", "-tags", ",".join(effective["tags"]),
                                 "./experimental/libbox"], cwd=source, env=env)
                            # Compile selected bindings directly: the official builder
                            # unconditionally enables many unrelated protocols.
                            run(bind_command(env, output, target, effective["tags"], fingerprint,
                                             pins, android_abis, args.apple_platform), cwd=source, env=env)
                            metadata = {"schema": 1, "mode": "selectable-core", "target": target,
                                        "identity": identity, "fingerprint": fingerprint,
                                        "features": effective["features"], "tags": effective["tags"],
                                        "toolchain": toolchain, "gomobile": mobile, "sdks": sdk_versions,
                                        "tailscale_dependency": tailscale, "wgc": wgc,
                                        "registry_sha256": hashlib.sha256(selected_registry.encode()).hexdigest()}
                            notices = legal_files(wgc_license=wgc_license)
                            notices = {**notices, "LICENSE.sing-box": source / "LICENSE",
                                       "NOTICE.mobile-core": ROOT / "native/mobile/NOTICE"}
                            artifacts[target] = publish_artifact(project, output, target, capabilities, metadata, notices)
                finally:
                    shutil.rmtree(stage)
            update_receipt(project, capabilities, selection, artifacts, pubspec_sha256)
            if "apple" in artifacts:
                invalidate_pods(project)
        print(f"Selectable core built. Receipt: {project / RECEIPT}")
        return 0
    except (BuildError, OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
