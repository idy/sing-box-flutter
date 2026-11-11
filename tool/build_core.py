#!/usr/bin/env python3
"""Build a host-selected native core without installing into the shared pub cache."""
from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
from build_libbox import (bootstrap_go, check_sdks, install_mobile, prepare_dependencies,
                          resolved_tailscale_dependency, retain_wgc_license, run, source_checkout)
from core_features import _unique_object, canonicalize, generate_registry, resolve_features
from native_common import (BuildError, ROOT, artifact_hashes, build_directory, legal_files,
                           load_pins, pin_identity, project_lock, sha256, write_json)
from wgc_overlay import load_extensions, wgc_overlay

ANDROID_TARGETS = {"armeabi-v7a": "android/arm", "arm64-v8a": "android/arm64",
                   "x86": "android/386", "x86_64": "android/amd64"}
RECEIPT = Path(".dart_tool/sing_box_flutter/native.json")
LINUX_ARCHITECTURES = {"amd64": 62, "arm64": 183}
ARTIFACT_NAMES = {"android": "libbox.aar", "apple": "Libbox.xcframework",
                  "macos": "Libbox.xcframework", "linux": "sing-box-core"}


def linux_architecture(requested: str | None = None) -> str:
    architecture = requested or {"x86_64": "amd64", "amd64": "amd64",
                                 "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine().lower())
    if architecture not in LINUX_ARCHITECTURES:
        raise BuildError("Linux builds require --arch amd64 or --arch arm64")
    return architecture


def artifact_key(target: str, arch: str | None = None) -> str:
    if target == "linux":
        if arch not in LINUX_ARCHITECTURES:
            raise BuildError("Linux artifact validation requires --arch amd64 or --arch arm64")
        return f"linux-{arch}"
    return target


def canonical_hash(value: dict) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def native_sources(directory: Path) -> dict[str, str]:
    if not directory.is_dir() or directory.is_symlink():
        raise BuildError("The selectable native core source is missing")
    result = {}
    for path in sorted(directory.rglob("*")):
        if path.is_symlink():
            raise BuildError("Selectable core source must not contain symlinks")
        if path.is_file() and not any(part.startswith(".") for part in path.relative_to(directory).parts):
            result[path.relative_to(directory).as_posix()] = sha256(path)
    if not any(name.endswith(".go") for name in result):
        raise BuildError("The selectable native core contains no Go source")
    return result


def mobile_sources(root: Path = ROOT) -> dict[str, str]:
    return native_sources(root / "native/mobile")


def linux_sources(root: Path = ROOT) -> dict[str, str]:
    return native_sources(root / "native/linux")


def canonical_macos(value: object) -> dict:
    if not isinstance(value, dict) or set(value) - {"mode"}:
        raise BuildError("sing_box_flutter.macos must contain only mode")
    mode = value.get("mode", "ne")
    if mode not in ("ne", "user_tun"):
        raise BuildError("sing_box_flutter.macos.mode must be ne or user_tun")
    return {"mode": mode}


def build_identity(selection: dict, extensions: list[dict], pins: dict, root: Path = ROOT,
                   *, macos: dict | None = None) -> dict:
    return {"schema": 1, "pins": pin_identity(pins, root), "selection": selection,
            **({"macos": canonical_macos(macos)} if macos is not None else {}),
            "mobile_sources": mobile_sources(root), "linux_sources": linux_sources(root),
            "tooling": {name: sha256(root / "tool" / name) for name in
                        ("build_core.py", "core_features.py", "build_libbox.py", "native_common.py",
                         "wgc_overlay.py", "source_inputs.py", "tailscale-go.json")},
            "extensions": [{key: value for key, value in entry.items() if key != "path"}
                           for entry in extensions]}


def load_host_build_request(path: Path, project: Path) -> tuple[dict, str, dict | None]:
    """Read the immutable Dart handoff, bound to exactly its parsed host bytes."""
    try:
        request = json.loads(path.read_text(), object_pairs_hook=_unique_object)
        if (not isinstance(request, dict) or
                set(request) not in ({"schema", "pubspec", "features"}, {"schema", "pubspec", "features", "macos"}) or
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
        macos = canonical_macos(request["macos"]) if "macos" in request else None
        return canonicalize(request["features"]), digest, macos
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


def copy_verified_sources(source: Path, destination: Path, expected_sources: dict[str, str],
                          changed_message: str) -> None:
    """Copy a captured source snapshot without importing its standalone module."""
    for name, digest in expected_sources.items():
        if name in ("go.mod", "go.sum"):
            continue
        path = destination / name
        path.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source / name, path)
        if sha256(path) != digest:
            raise BuildError(changed_message)


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
        copy_verified_sources(root / "native/mobile", original, expected_sources,
                              "Mobile core source changed while capturing the build")
        (original / "registry_selected.go").write_text(registry)
        yield
    finally:
        if moved:
            if original.exists():
                shutil.rmtree(original)
            backup.rename(original)
        shutil.rmtree(backup_root)


@contextlib.contextmanager
def linux_overlay(source: Path, expected_sources: dict[str, str], root: Path = ROOT):
    """Compile the verified helper inside the pinned module, then remove it."""
    if linux_sources(root) != expected_sources:
        raise BuildError("Linux helper source changed after the build identity was captured")
    directory = source / "cmd/sing-box-flutter-linux"
    if directory.exists() or directory.is_symlink():
        raise BuildError("Linux helper overlay would overwrite existing core source")
    directory.mkdir()
    try:
        copy_verified_sources(root / "native/linux", directory, expected_sources,
                              "Linux helper source changed while capturing the build")
        yield "./cmd/sing-box-flutter-linux"
    finally:
        shutil.rmtree(directory)


@contextlib.contextmanager
def linux_clash_overlay(source: Path):
    """Make the pinned external-UI download honour Linux service cancellation."""
    patches = {
        "experimental/clashapi/server_resources.go": (
            ("\tresponse, err := httpClient.Get(downloadURL)",
             "\trequest, err := http.NewRequestWithContext(s.ctx, http.MethodGet, downloadURL, nil)\n"
             "\tif err != nil {\n\t\treturn err\n\t}\n"
             "\tresponse, err := httpClient.Do(request)"),
            ("\tif err != nil {\n\t\tremoveAllInDirectory(s.ctx, s.externalUI)",
             "\tif err != nil && s.ctx.Err() == nil {\n\t\tremoveAllInDirectory(s.ctx, s.externalUI)"),
        ),
        "experimental/clashapi/server.go": (
            ("\t\ts.checkAndDownloadExternalUI()\n\t\tvar (",
             "\t\tscope.Add(s.httpServer.Close)\n\t\ts.checkAndDownloadExternalUI()\n"
             "\t\tif err := scope.Context().Err(); err != nil {\n\t\t\treturn err\n\t\t}\n\t\tvar ("),
            ("\t\tscope.Add(s.httpServer.Close)\n\t\ts.logger.Info(", "\t\ts.logger.Info("),
        ),
    }
    originals = {}
    replacements = {}
    for name, changes in patches.items():
        path = source / name
        original = path.read_bytes()
        content = original.decode()
        for old, new in changes:
            if content.count(old) != 1:
                raise BuildError("Pinned Clash cancellation source differs from the reviewed input")
            content = content.replace(old, new, 1)
        originals[path] = original
        replacements[path] = content.encode()
    try:
        for path, content in replacements.items():
            path.write_bytes(content)
        yield
    finally:
        for path, original in originals.items():
            path.write_bytes(original)


def linker_flags(fingerprint: str, pins: dict) -> str:
    return ("-s -w -buildid= -checklinkname=0 "
            f"-X github.com/sagernet/sing-box/constant.Version={pins['sing_box_version']} "
            f"-X github.com/sagernet/sing-box/experimental/libbox.coreFingerprint={fingerprint}")


def linux_command(go: Path, output: Path, tags: list[str], fingerprint: str,
                  pins: dict, package: str) -> list[str | Path]:
    return [go, "build", "-trimpath", "-buildvcs=false", "-ldflags", linker_flags(fingerprint, pins),
            "-tags", ",".join(sorted(set(tags) | {"badlinkname", "tfogo_checklinkname0"})),
            "-o", output, package]


def bind_command(env: dict, output: Path, target: str, tags: list[str],
                 fingerprint: str, pins: dict, android_abis: list[str], apple_platform: str) -> list[str | Path]:
    linker = linker_flags(fingerprint, pins)
    command: list[str | Path] = [Path(env["GOBIN"]) / "gomobile", "bind", "-v", "-o", output]
    if target == "android":
        command += ["-target", ",".join(ANDROID_TARGETS[name] for name in android_abis),
                    "-androidapi", "24", "-javapkg=io.nekohasekai", "-libname=box"]
    elif target == "macos":
        command += ["-target", "macos/arm64,macos/amd64", "-libname=box", "-macosversion=12.0"]
    else:
        command += ["-target", apple_platform, "-libname=box", "-iosversion=15.0",
                    "-tags-not-macos=with_low_memory"]
    command += ["-trimpath", "-buildvcs=false", "-ldflags", linker,
                "-tags", ",".join(sorted(set(tags) | {"badlinkname", "tfogo_checklinkname0"})),
                "./experimental/libbox"]
    return command


def validate_artifact(path: Path, target: str, arch: str | None = None) -> None:
    if target == "linux":
        # Validate without executing a cross-compiled artifact on the build host.
        artifact_key(target, arch)
        if path.is_symlink() or not path.is_file() or not path.stat().st_mode & 0o111:
            raise BuildError("The selected Linux helper must be a regular executable file")
        with path.open("rb") as stream:
            header = stream.read(64)
        if (len(header) != 64 or header[:7] != b"\x7fELF\x02\x01\x01" or
                int.from_bytes(header[16:18], "little") not in (2, 3) or
                int.from_bytes(header[18:20], "little") != LINUX_ARCHITECTURES[arch] or
                int.from_bytes(header[20:24], "little") != 1 or
                int.from_bytes(header[52:54], "little") != 64):
            raise BuildError("The selected Linux helper is not a valid ELF executable for the requested architecture")
    elif target == "android":
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


RECIPE_METADATA = ("schema", "mode", "target", "arch", "environment", "identity", "fingerprint",
                   "features", "tags", "artifact_sha256")
WORK_MARKER = ".sing-box-flutter-work"


def work_root(project: Path, requested: str | None, *, create: bool = True) -> Path:
    """Use the package .build, or a host-local directory when the pub cache is read-only."""
    if requested is not None:
        path = Path(requested).expanduser()
        if not path.is_absolute() or path.is_symlink():
            raise BuildError("--work-dir must be an absolute path that is not a symlink")
        path, package = path.resolve(), ROOT.resolve()
        if path.is_relative_to(package) and not path.is_relative_to(package / ".build"):
            raise BuildError("--work-dir must not be inside the plugin sources")
        if path.exists() and (not path.is_dir() or
                              (any(path.iterdir()) and not (path / WORK_MARKER).is_file())):
            raise BuildError("--work-dir must be empty or an existing sing_box_flutter work directory")
        if create:
            path.mkdir(parents=True, exist_ok=True)
            (path / WORK_MARKER).touch()
        return path
    package = build_directory(ROOT / ".build", ROOT)
    try:
        if create:
            package.mkdir(parents=True, exist_ok=True)
        if os.access(package if package.exists() else ROOT, os.W_OK | os.X_OK):
            return package
    except OSError:
        pass
    fallback = host_path(project, ".dart_tool/sing_box_flutter/work")
    if create:
        fallback.mkdir(parents=True, exist_ok=True)
    return fallback.resolve()


def work_directory(path: str | Path, work: Path) -> Path:
    result = Path(path).expanduser().absolute().resolve()
    if not result.is_relative_to(work):
        raise BuildError("Bootstrap and cache directories must be inside the native work directory")
    return result


def _tree(path: Path) -> dict[str, str] | None:
    if not path.exists() and not path.is_symlink():
        return {}
    try:
        return artifact_hashes(path)
    except BuildError:
        return None


def repair_provenance(final: Path, stage: Path, metadata: dict) -> None:
    """Reuse identical binary bytes, never provenance that no longer describes them."""
    current = final / "native-build.json"
    try:
        existing = json.loads(current.read_text())
    except (OSError, ValueError):
        existing = None
    if (current.is_symlink() or not isinstance(existing, dict) or
            any(existing.get(key) != metadata.get(key) for key in RECIPE_METADATA)):
        (stage / "native-build.json").replace(current)
    legal, staged = final / "Legal", stage / "Legal"
    if _tree(legal) != _tree(staged):
        retired = Path(tempfile.mkdtemp(prefix=".retired-", dir=final.parent))
        try:
            if legal.is_symlink() or legal.is_file():
                legal.unlink()
            elif legal.exists():
                legal.rename(retired / "Legal")
            if staged.exists():
                staged.rename(legal)
        finally:
            shutil.rmtree(retired)


def publish_artifact(project: Path, output: Path, target: str, capabilities: dict,
                     metadata: dict, notices: dict[str, Path], *, arch: str | None = None) -> dict:
    """Publish an immutable content-addressed artifact before updating the pointer."""
    validate_artifact(output, target, arch)
    key = artifact_key(target, arch)
    hashes = artifact_hashes(output)
    metadata = {**metadata, "artifact_sha256": hashes}
    if target == "linux":
        metadata["arch"] = arch
    content = canonical_hash(hashes)
    relative = Path(".dart_tool/sing_box_flutter/builds") / capabilities["fingerprint"] / key / content
    final = host_path(project, relative.as_posix())
    final.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=".stage-", dir=final.parent))
    name = ARTIFACT_NAMES[target]
    try:
        # Stage small provenance/notice inputs even on a cache hit. Keep the first
        # provenance while it still describes these bytes; repair corrupt metadata
        # and stale notices without copying a whole duplicate SDK.
        write_json(stage / "core-capabilities.json", capabilities)
        write_json(stage / "native-build.json", metadata)
        for filename, path in notices.items():
            destination = stage / "Legal" / filename
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, destination)
        if final.exists():
            if target == "linux":
                validate_artifact(final / name, target, arch)
            if artifact_hashes(final / name) != hashes or json.loads((final / "core-capabilities.json").read_text()) != capabilities:
                raise BuildError("An existing content-addressed artifact was modified")
            if target == "android":
                asset = host_path(project, (relative / "assets/sing_box_flutter/core-capabilities.json").as_posix())
                if not asset.is_file() or sha256(asset) != sha256(final / "core-capabilities.json"):
                    raise BuildError("An existing Android capabilities asset is missing or changed; remove that cached artifact and rebuild")
            repair_provenance(final, stage, metadata)
        else:
            if output.is_dir():
                shutil.copytree(output, stage / name, symlinks=True)
            else:
                shutil.copyfile(output, stage / name)
                if target == "linux":
                    shutil.copymode(output, stage / name)
                    validate_artifact(stage / name, target, arch)
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
            "sha256": hashes, "capabilities_sha256": sha256(final / "core-capabilities.json"),
            **({"arch": arch} if target == "linux" else {})}


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
        if target.startswith("linux-"):
            if target != artifact_key("linux", entry.get("arch")):
                raise BuildError("A previously built Linux artifact has an invalid architecture")
            validate_artifact(host_path(project, entry["path"]), "linux", entry["arch"])
    updated = {**capabilities, "requested_features": requested,
               "pubspec_sha256": current_pubspec,
               "artifacts": {**retained, **artifacts}}
    for target, platform in (("apple", "ios"), ("macos", "macos")):
        selection = _pod_selection(updated, target)
        if (_pod_selection(previous, target) != selection or
                (selection is not None and _installed_pod_selection(project, platform) != selection)):
            # Refresh before publishing the new pointer. If refresh fails, the
            # old receipt remains so a retry cannot mistake stale Pods for current.
            invalidate_pods(project, platform)
    write_json(path, updated)


def _pod_selection(receipt: dict, target: str) -> dict | None:
    artifacts = receipt.get("artifacts")
    entry = artifacts.get(target) if isinstance(artifacts, dict) else None
    if entry is None:
        return None
    return {"schema": 1, "target": target, "fingerprint": receipt.get("fingerprint"),
            "pubspec_sha256": receipt.get("pubspec_sha256"),
            "macos": receipt.get("macos"), "artifact": entry}


def _installed_pod_selection(project: Path, platform: str) -> dict | None:
    """Trust a guard marker only when CocoaPods' installed locks pin its podspec."""
    pods = project / platform / "Pods"
    manifest = pods / "Manifest.lock"
    lock = pods.parent / "Podfile.lock"
    podspec = pods / "Local Podspecs/sing_box_flutter.podspec.json"
    try:
        for path in (manifest, lock, podspec):
            if path.is_symlink() or not path.is_file() or not path.resolve().is_relative_to(project.resolve()):
                return None
        installed_lock = manifest.read_bytes()
        if installed_lock != lock.read_bytes():
            return None
        spec_bytes = podspec.read_bytes()
        # Read only CocoaPods' generated checksum section, without a YAML dependency.
        checksums = re.search(r"(?m)^SPEC CHECKSUMS:\n((?:  .*\n)+)", installed_lock.decode())
        if (checksums is None or
                re.findall(r"(?m)^  sing_box_flutter: ([0-9a-f]{40})$", checksums[1]) !=
                [hashlib.sha1(spec_bytes).hexdigest()]):
            return None
        spec = json.loads(spec_bytes, object_pairs_hook=_unique_object)
        shared = next(part for part in spec["subspecs"] if part["name"] == "Shared")
        phases = shared.get("script_phases", [])
        if isinstance(phases, dict):
            phases = [phases]
        guards = [phase for phase in phases if phase.get("name") == "Validate selected core artifacts"]
        if (len(guards) != 1 or guards[0].get("always_out_of_date") != "1" or
                guards[0].get("execution_position") != "before_compile"):
            return None
        marker = "# sing_box_flutter pod selection: "
        selections = [line[len(marker):] for line in guards[0]["script"].splitlines() if line.startswith(marker)]
        if len(selections) == 1:
            return json.loads(selections[0], object_pairs_hook=_unique_object)
    except (OSError, ValueError, KeyError, TypeError, AttributeError, StopIteration):
        pass
    return None


def invalidate_pods(project: Path, platform: str = "ios") -> None:
    """Make Flutter refresh dynamic podspec paths without unlocking pod versions."""
    if platform not in ("ios", "macos"):
        raise BuildError("Unsupported CocoaPods platform")
    manifest = project / platform / "Pods/Manifest.lock"
    if not manifest.exists() and not manifest.is_symlink():
        return
    if (manifest.is_symlink() or not manifest.is_file() or
            not manifest.resolve().is_relative_to(project.resolve())):
        raise BuildError("Cannot refresh CocoaPods metadata outside this host; run pod install manually")
    # This generated sandbox receipt is disposable. Keep the host Podfile.lock;
    # Flutter detects its missing counterpart and runs pod install on next build.
    manifest.read_bytes()
    manifest.unlink()


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--features-file", type=Path, required=True)
    result.add_argument("--project-dir", type=Path, required=True)
    result.add_argument("--target", choices=("apple", "android", "macos", "linux", "all"), default="all",
                        help="Native platform (all builds Android, iOS and macOS)")
    result.add_argument("--arch", choices=tuple(LINUX_ARCHITECTURES),
                        help="Linux helper architecture (default: build host architecture)")
    result.add_argument("--android-abis", default=",".join(ANDROID_TARGETS))
    result.add_argument("--apple-platform", choices=("ios", "ios,iossimulator"), default="ios,iossimulator")
    result.add_argument("--wgc-config", type=Path)
    result.add_argument("--work-dir", help="Absolute writable directory for the verified toolchain, "
                        "source checkout and caches (default: plugin .build, or the host's "
                        ".dart_tool/sing_box_flutter/work when the package is read-only)")
    result.add_argument("--bootstrap-dir")
    result.add_argument("--cache-dir")
    result.add_argument("--dry-run", action="store_true")
    result.add_argument("--bootstrap-only", action="store_true")
    return result


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        project = args.project_dir.expanduser().resolve(strict=True)
        if not (project / "pubspec.yaml").is_file():
            raise BuildError("--project-dir must name the Flutter host project containing pubspec.yaml")
        selection, pubspec_sha256, macos = load_host_build_request(args.features_file, project)
        if args.arch is not None and args.target != "linux":
            raise BuildError("--arch is only supported with --target linux")
        arch = linux_architecture(args.arch) if args.target == "linux" else None
        if args.target != "linux" and "tun" not in selection["inbounds"]:
            raise BuildError("A mobile VPN core must include the tun inbound")
        pins = load_pins()
        extensions = load_extensions(args.wgc_config)
        if extensions and "wgc" not in selection.get("endpoints", []):
            raise BuildError("Compiled WGC transports require the wgc endpoint to be selected")
        android_abis = args.android_abis.split(",")
        if (not android_abis or len(android_abis) != len(set(android_abis)) or
                any(name not in ANDROID_TARGETS for name in android_abis)):
            raise BuildError("--android-abis must list unique supported Android ABI names")
        work = work_root(project, args.work_dir, create=not args.dry_run)
        bootstrap = work_directory(args.bootstrap_dir or work / "bootstrap", work)
        cache = work_directory(args.cache_dir or work / "cache", work)
        if bootstrap == cache or bootstrap.is_relative_to(cache) or cache.is_relative_to(bootstrap):
            raise BuildError("Bootstrap and cache directories must be separate, non-nested work directories")
        targets = ["android", "apple", "macos"] if args.target == "all" else [args.target]
        resolved = {target: resolve_features(selection, target) for target in targets}
        identity = build_identity(selection, extensions, pins, macos=macos)
        fingerprint = canonical_hash(identity)
        registry = generate_registry(next(iter(resolved.values()))["features"], extensions)
        if args.dry_run:
            print(json.dumps({"mode": "selectable-core", "version": pins["sing_box_version"],
                              "fingerprint": fingerprint, "selection": selection, "resolved": resolved,
                              "registry_sha256": hashlib.sha256(registry.encode()).hexdigest(),
                              "receipt": RECEIPT.as_posix(), "android_abis": android_abis,
                              "apple_platform": args.apple_platform, "macos": macos or {"mode": "ne"},
                              "arch": arch,
                              "shared_pub_cache_artifacts": False}, indent=2))
            return 0
        # The work lock protects its verified toolchain/source; the host lock
        # also protects per-project receipts across different pub-cache versions.
        with contextlib.ExitStack() as locks:
            locks.enter_context(project_lock(base=work))
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
            mobile = tailscale = None
            if targets == ["linux"]:
                env = {**env, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch}
                # Linux dependencies must be resolved after both selected source
                # overlays are installed, including for --bootstrap-only.
            else:
                mobile, tailscale = install_mobile(go, env, source, pins)
            if args.bootstrap_only and targets != ["linux"]:
                print("Verified selectable-core compiler and dependencies are ready.")
                return 0
            artifacts = {}
            for target in targets:
                effective = resolved[target]
                capabilities = {"schema": 1, "version": pins["sing_box_version"],
                                "fingerprint": fingerprint, "features": effective["features"]}
                stage = Path(tempfile.mkdtemp(prefix=f"selectable-{target}-", dir=cache))
                output = stage / ARTIFACT_NAMES[target]
                try:
                    selected_registry = generate_registry(effective["features"], extensions)
                    with mobile_overlay(source, selected_registry, identity["mobile_sources"]):
                        with contextlib.ExitStack() as stack:
                            wgc = None
                            wgc_license = None
                            if "wgc" in effective["features"]["endpoints"]:
                                wgc = stack.enter_context(wgc_overlay(source, go, env, run, extensions))
                                wgc_license = retain_wgc_license(go, env, source, cache, pins)
                            # Compile directly: the official mobile builder enables
                            # unrelated protocols. Linux uses the same selected libbox.
                            if target == "linux":
                                if "clash_api" in effective["features"]["api"]:
                                    stack.enter_context(linux_clash_overlay(source))
                                package = stack.enter_context(linux_overlay(source, identity["linux_sources"]))
                                tailscale = prepare_dependencies(go, env, source, pins,
                                    packages=["./experimental/libbox", package], tags=effective["tags"])
                                if args.bootstrap_only:
                                    print("Verified selectable-core compiler and dependencies are ready.")
                                    return 0
                                if platform.system() == "Linux" and linux_architecture() == arch:
                                    run([go, "test", "-tags", ",".join(effective["tags"]),
                                         "./experimental/libbox", package], cwd=source, env=env)
                                run(linux_command(go, output, effective["tags"], fingerprint, pins, package),
                                    cwd=source, env=env)
                            else:
                                if any("tailscale" in values for values in effective["features"].values()):
                                    tailscale = resolved_tailscale_dependency(go, env, source, pins)
                                # Mobile constructor tests run on the build host;
                                # Linux-only graphs are tested on matching Linux hosts.
                                run([go, "test", "-tags", ",".join(effective["tags"]),
                                     "./experimental/libbox"], cwd=source, env=env)
                                run(bind_command(env, output, target, effective["tags"], fingerprint,
                                                 pins, android_abis, args.apple_platform), cwd=source, env=env)
                            metadata = {"schema": 1, "mode": "selectable-core", "target": target,
                                        "identity": identity, "fingerprint": fingerprint,
                                        "features": effective["features"], "tags": effective["tags"],
                                        "toolchain": toolchain, "sdks": sdk_versions,
                                        "tailscale_dependency": tailscale, "wgc": wgc,
                                        "registry_sha256": hashlib.sha256(selected_registry.encode()).hexdigest()}
                            if target == "linux":
                                metadata.update({"arch": arch, "environment": {
                                    "GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"}})
                            else:
                                metadata["gomobile"] = mobile
                            notices = legal_files(wgc_license=wgc_license)
                            notices = {**notices, "LICENSE.sing-box": source / "LICENSE",
                                       "NOTICE.mobile-core": ROOT / "native/mobile/NOTICE"}
                            artifacts[artifact_key(target, arch)] = publish_artifact(
                                project, output, target, capabilities, metadata, notices, arch=arch)
                finally:
                    shutil.rmtree(stage)
            receipt_capabilities = {**capabilities, **({"macos": macos} if macos is not None else {})}
            update_receipt(project, receipt_capabilities, selection, artifacts, pubspec_sha256)
        print(f"Selectable core built. Receipt: {project / RECEIPT}")
        return 0
    except (BuildError, OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
