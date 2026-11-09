"""Shared validation for the pinned libbox build and release installer (Python 3.10+)."""
from __future__ import annotations

import contextlib
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import tarfile
import tempfile
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
CORE_REPO = "https://github.com/SagerNet/sing-box.git"
RELEASE_REPO = "idy/sing-box-flutter"
GOMOBILE_MODULE = "github.com/sagernet/gomobile"
TS_OMIT_TAGS = (
    "ts_omit_logtail", "ts_omit_ssh", "ts_omit_drive", "ts_omit_taildrop",
    "ts_omit_webclient", "ts_omit_doctor", "ts_omit_capture", "ts_omit_kube",
    "ts_omit_aws", "ts_omit_synology", "ts_omit_bird",
)
UPSTREAM_REQUIRED_TAGS = ("with_tailscale", "with_gvisor", "with_quic", "with_wireguard",
                          "with_utls", "with_clash_api", *TS_OMIT_TAGS)
REQUIRED_TAGS = (*UPSTREAM_REQUIRED_TAGS, "with_wgc")
ARTIFACTS = {"android": "android/libs/libbox.aar", "apple": "ios/Frameworks/Libbox.xcframework"}


class BuildError(RuntimeError):
    pass


def load_pins(root: Path = ROOT) -> dict:
    pins = json.loads((root / "native-core.json").read_text())
    for key in ("sing_box_revision", "tailscale_go_revision"):
        if not re.fullmatch(r"[0-9a-f]{40}", pins.get(key, "")):
            raise BuildError(f"{key} must be a full lowercase git revision")
    for key in ("sing_box_version", "gomobile_version"):
        if not re.fullmatch(r"v?\d+\.\d+\.\d+", pins.get(key, "")):
            raise BuildError(f"Invalid {key}")
    lock = json.loads((root / "tool/tailscale-go.json").read_text())
    if lock["revision"] != pins["tailscale_go_revision"]:
        raise BuildError("Toolchain checksum lock does not match native-core.json")
    if pins.get("android_min_sdk") != 24 or pins.get("ios_min_version") != "15.0":
        raise BuildError("Pinned upstream builder requires Android 24 / iOS 15.0")
    return pins


def pin_identity(pins: dict) -> dict:
    from wgc_overlay import source_digest
    identity = {key: pins[key] for key in ("sing_box_version", "sing_box_revision",
                                          "tailscale_go_revision", "gomobile_version", "wgc_version")}
    identity["wgc_source_sha256"] = source_digest(ROOT / "native/wgc")
    identity["wgc_overlay_sha256"] = sha256(ROOT / "tool/wgc_overlay.py")
    return identity


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_json(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    temporary.replace(path)


def build_directory(path: str | Path, root: Path = ROOT) -> Path:
    result = Path(path).expanduser().absolute().resolve()
    boundary = root.resolve() / ".build"
    # Refuse .build itself resolving through a symlink outside the project.
    if boundary.is_symlink() or not result.is_relative_to(boundary):
        raise BuildError("Bootstrap and cache directories must be inside project .build")
    return result


@contextlib.contextmanager
def project_lock(root: Path = ROOT):
    import fcntl  # Builds support macOS/Linux, including Android cross builds.
    base = build_directory(root / ".build", root)
    base.mkdir(parents=True, exist_ok=True)
    with (base / "native-core.lock").open("w") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise BuildError("Another native build/install is using this project") from exc
        yield


def download(url: str, directory: Path, expected: str | None = None,
             max_bytes: int = 4 * 1024**3) -> tuple[Path, dict]:
    """Every response goes into its own fresh directory, never used as cwd."""
    if urllib.parse.urlparse(url).scheme != "https":
        raise BuildError("Downloads require HTTPS")
    directory.mkdir(parents=True, exist_ok=True)
    isolated = Path(tempfile.mkdtemp(prefix="download-", dir=directory))
    path = isolated / "payload"
    request = urllib.request.Request(url, headers={"User-Agent": "sing-box-flutter-native-build"})
    try:
        with urllib.request.urlopen(request, timeout=60) as response, path.open("xb") as out:
            if urllib.parse.urlparse(response.url).scheme != "https":
                raise BuildError("Download redirected away from HTTPS")
            size = 0
            while chunk := response.read(1024 * 1024):
                size += len(chunk)
                if size > max_bytes:
                    raise BuildError("Download exceeds size limit")
                out.write(chunk)
        digest = sha256(path)
        if expected is not None and digest != expected:
            raise BuildError("Downloaded SHA-256 does not match the expected manifest/lock")
        return path, {"url": url, "sha256": digest, "size": size}
    except Exception:
        shutil.rmtree(isolated)
        raise


def _archive_path(name: str) -> PurePosixPath:
    path = PurePosixPath(name)
    if (not name or "\\" in name or "\x00" in name or path.is_absolute()
            or ".." in path.parts or (path.parts and ":" in path.parts[0])):
        raise BuildError("Unsafe archive member path")
    return path


def safe_extract(archive: Path, destination: Path, max_bytes: int = 4 * 1024**3) -> None:
    """Extract only into an empty directory; no devices, hardlinks or link traversal.

    Framework symlinks are allowed only if they resolve inside the extraction root.
    Files are created before symlinks so archive order cannot redirect writes.
    """
    if destination.is_symlink() or (destination.exists() and any(destination.iterdir())):
        raise BuildError("Extraction destination must be a new empty directory")
    destination.mkdir(parents=True, exist_ok=True)
    with tarfile.open(archive, "r:gz") as bundle:
        members = bundle.getmembers()
        if len(members) > 200_000 or sum(m.size for m in members) > max_bytes:
            raise BuildError("Archive exceeds extraction limits")
        names = set()
        symlinks = set()
        for member in members:
            path = _archive_path(member.name)
            if str(path) == "." and member.isdir():
                continue
            if path in names:
                raise BuildError("Duplicate archive member")
            names.add(path)
            if member.issym():
                target = PurePosixPath(member.linkname)
                if target.is_absolute() or "\\" in member.linkname or ":" in member.linkname:
                    raise BuildError("Unsafe archive symlink")
                # Resolve '..' lexically without following links that are not created yet.
                depth = len(path.parent.parts)
                for part in target.parts:
                    depth += -1 if part == ".." else (0 if part == "." else 1)
                    if depth < 0:
                        raise BuildError("Archive symlink escapes extraction root")
                symlinks.add(path)
            elif not (member.isfile() or member.isdir()):
                raise BuildError("Unsupported archive member type")
        for path in names:
            if any(parent in symlinks for parent in path.parents):
                raise BuildError("Archive writes through a symlink")
        for member in members:
            path = destination / member.name
            if member.isdir():
                path.mkdir(parents=True, exist_ok=True)
            elif member.isfile():
                path.parent.mkdir(parents=True, exist_ok=True)
                source = bundle.extractfile(member)
                if source is None:
                    raise BuildError("Unreadable archive file")
                with source, path.open("xb") as out:
                    shutil.copyfileobj(source, out)
                path.chmod(0o755 if member.mode & 0o111 else 0o644)
        for member in members:
            if member.issym():
                path = destination / member.name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.symlink_to(member.linkname)
        for path in symlinks:
            try:
                resolved = (destination / str(path)).resolve(strict=True)
            except (OSError, RuntimeError) as exc:
                raise BuildError("Dangling or cyclic archive symlink") from exc
            if not resolved.is_relative_to(destination.resolve()):
                raise BuildError("Archive symlink escapes extraction root")


def parse_manifest(text: str) -> dict[str, str]:
    entries = {}
    for line in text.splitlines():
        if not line.strip():
            continue
        match = re.fullmatch(r"([0-9a-f]{64}) [ *]([^/\\\s]+)", line)
        if not match or match[2] in entries or match[2] in (".", ".."):
            raise BuildError("Invalid SHA256SUMS manifest")
        entries[match[2]] = match[1]
    if not entries:
        raise BuildError("Empty SHA256SUMS manifest")
    return entries


def artifact_hashes(path: Path) -> dict[str, str]:
    if path.is_symlink():
        raise BuildError("Artifact root must not be a symlink")
    if path.is_file():
        return {path.name: sha256(path)}
    if not path.is_dir():
        raise BuildError("Expected native artifact is absent")
    for entry in path.rglob("*"):
        if entry.is_symlink() and not entry.resolve(strict=True).is_relative_to(path.resolve()):
            raise BuildError("Framework symlink escapes artifact root")
    hashes = {}
    for entry in sorted(path.rglob("*")):
        name = entry.relative_to(path).as_posix()
        if entry.is_symlink():
            hashes[name] = hashlib.sha256(("symlink:" + os.readlink(entry)).encode()).hexdigest()
        elif entry.is_file():
            hashes[name] = sha256(entry)
    return hashes


def legal_files(root: Path = ROOT, *, wgc_license: Path | None = None) -> dict[str, Path]:
    files = {"LICENSE": root / "LICENSE", "THIRD_PARTY_NOTICES.md": root / "THIRD_PARTY_NOTICES.md"}
    if wgc_license is not None:
        if wgc_license.is_symlink() or not wgc_license.is_file():
            raise BuildError("Pinned WGC license is missing; rebuild the native artifact")
        files["LICENSE.wgc"] = wgc_license
    return files


def archive_name(pins: dict, target: str) -> str:
    return f"libbox-{pins['sing_box_version']}-{target}.tar.gz"


def validate_metadata(metadata: dict, pins: dict, target: str, *, allow_custom: bool = False) -> None:
    if (metadata.get("schema") != 1 or metadata.get("target") != target
            or metadata.get("pins") != pin_identity(pins)):
        raise BuildError("Release metadata does not match project pins/target")
    dependency = metadata.get("tailscale_dependency", {})
    if pins["tailscale_go_revision"] not in (dependency.get("toolchain_revision"), dependency.get("next_toolchain_revision")):
        raise BuildError("Release Tailscale dependency runtime contract differs from the toolchain pin")
    wgc = metadata.get("wgc", {})
    expected = pin_identity(pins)
    if (wgc.get("source_sha256") != expected["wgc_source_sha256"]
            or wgc.get("overlay_sha256") != expected["wgc_overlay_sha256"]):
        raise BuildError("Release WGC source/registration overlay differs from this package")
    if not allow_custom and wgc.get("extensions") != []:
        raise BuildError("Custom WGC extensions require a local build, not the official release installer")
    tags = metadata.get("tags", [])
    if not set(REQUIRED_TAGS).issubset(tags):
        raise BuildError("Release metadata is missing required mobile features")
    if target == "apple" and "with_low_memory" not in tags:
        raise BuildError("Apple release metadata is missing with_low_memory")
    profile = json.loads((ROOT / "tool/mobile-profile.json").read_text())
    provenance = metadata.get("mobile_profile", {})
    if (provenance.get("profile_sha256") != sha256(ROOT / "tool/mobile-profile.json")
            or provenance.get("upstream_builder_sha256") != profile["upstream_builder_sha256"]
            or provenance.get("removed_tags") != profile["drop_tags"]
            or any(tag in tags for tag in profile["drop_tags"])):
        raise BuildError("Release metadata does not match the reviewed mobile feature profile")
