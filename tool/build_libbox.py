#!/usr/bin/env python3
"""Build pinned upstream libbox with verified Tailscale Go. No stock Go fallback."""
from __future__ import annotations

import argparse
import contextlib
import hashlib
import gzip
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error

# Keep bytecode and all tool state out of source / the pub archive.
sys.dont_write_bytecode = True
from native_common import (ARTIFACTS, BuildError, CORE_REPO, GOMOBILE_MODULE,
                           REQUIRED_TAGS, TRANSIENT_HTTP_STATUSES, UPSTREAM_REQUIRED_TAGS, ROOT, archive_name, artifact_hashes, build_directory,
                           download, legal_files, load_pins, pin_identity, project_lock,
                           safe_extract, sha256, validate_metadata, write_json)
from wgc_overlay import load_extensions, wgc_overlay

NDK_VERSION = "28.0.13004108"  # build_shared.FindSDK prefers this exact version.


def run(args: list[str | Path], *, cwd: Path = ROOT, env: dict | None = None,
        capture: bool = False) -> str:
    # Never place a download/extraction directory on Python's import path or cwd.
    print("+ " + shlex.join(str(arg) for arg in args), flush=True)
    result = subprocess.run([str(arg) for arg in args], cwd=cwd, env=env, check=True,
                            text=True, encoding="utf-8", stdout=subprocess.PIPE if capture else None)
    return result.stdout.strip() if capture else ""


def host_key() -> str:
    machine = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64",
               "amd64": "amd64"}.get(platform.machine().lower())
    system = platform.system().lower()
    if system not in ("darwin", "linux", "windows") or machine is None:
        raise BuildError("Build host must be macOS/Linux/Windows arm64 or amd64")
    return f"{system}-{machine}"


def go_environment(goroot: Path, cache: Path, inherited: dict | None = None) -> dict:
    """Pin the compiler and caches while retaining explicit module-access rules."""
    original = os.environ if inherited is None else inherited
    env = {k: v for k, v in original.items()
           if not k.startswith("GO") and not k.startswith("CGO_")
           and k not in ("CC", "CXX", "AR", "PKG_CONFIG")}
    env.update({
        "GOROOT": str(goroot), "GOTOOLCHAIN": "local", "GOENV": "off", "GOWORK": "off",
        "GOPATH": str(cache / "gopath"), "GOBIN": str(cache / "gopath/bin"),
        "GOCACHE": str(cache / "go-build"), "GOMODCACHE": str(cache / "go-mod"),
        "GOTMPDIR": str(cache / "tmp"), "TMPDIR": str(cache / "tmp"),
        "TMP": str(cache / "tmp"), "TEMP": str(cache / "tmp"),
        "TEST_TELEMETRY_DIR": str(cache / "telemetry"),
        "GOPROXY": original.get("GOPROXY", "https://proxy.golang.org,direct"),
        "GOSUMDB": "sum.golang.org",
        "GOFLAGS": "-mod=readonly -trimpath -buildvcs=false", "CGO_ENABLED": "1",
        "PATH": os.pathsep.join((str(goroot / "bin"), str(cache / "gopath/bin"),
                                 original.get("PATH", os.defpath))),
    })
    # Scope private-module access without accepting compiler/workspace overrides.
    # Leave omitted values unset so Go can derive their defaults from GOPRIVATE.
    for name in ("GOPRIVATE", "GONOPROXY", "GONOSUMDB"):
        if name in original:
            env[name] = original[name]
    for name in ("gopath/bin", "go-build", "go-mod", "tmp", "telemetry"):
        (cache / name).mkdir(parents=True, exist_ok=True)
    # The Go command's documented test hook also controls compiler counters.
    # Keep even telemetry's disabled mode file inside the project cache.
    (cache / "telemetry/mode").write_text("off\n", encoding="utf-8")
    return env


def bootstrap_go(pins: dict, bootstrap: Path, cache: Path) -> tuple[Path, dict, dict]:
    lock = json.loads((ROOT / "tool/tailscale-go.json").read_text(encoding="utf-8"))
    key = host_key()
    digest = lock["sha256"][key]
    url = (f"https://github.com/tailscale/go/releases/download/"
           f"build-{pins['tailscale_go_revision']}/{key}.tar.gz")
    asset_url = f"https://api.github.com/repos/tailscale/go/releases/assets/{lock['asset_ids'][key]}"
    cached_archive = cache / "archives" / f"tailscale-go-{pins['tailscale_go_revision']}-{key}.tar.gz"
    cached_source = cached_archive.with_suffix(".json")
    cached_archive.parent.mkdir(parents=True, exist_ok=True)
    if cached_archive.exists():
        if sha256(cached_archive) != digest:
            raise BuildError("Cached Tailscale Go archive failed SHA-256 verification; remove it and retry")
        # Older caches have no source receipt; do not invent a browser-download origin.
        source_url = json.loads(cached_source.read_text(encoding="utf-8")).get("url") if cached_source.exists() else None
        provenance = {"url": source_url if source_url in (url, asset_url) else None,
                      "sha256": digest, "size": cached_archive.stat().st_size, "cached": True}
    else:
        try:
            response, provenance = download(url, bootstrap / "downloads", digest)
        except urllib.error.HTTPError as error:
            if error.code not in TRANSIENT_HTTP_STATUSES:
                raise
            print(f"Browser download exhausted HTTP {error.code}; trying official GitHub asset API", flush=True)
            response, provenance = download(asset_url, bootstrap / "downloads", digest,
                                            headers={"Accept": "application/octet-stream"})
        shutil.copyfile(response, cached_archive)
        # Only the stable official request URL is retained, never a signed redirect.
        write_json(cached_source, provenance)
    # Re-extract a verified archive on EVERY invocation. An edited cached executable
    # or forged local stamp can never be selected before verification.
    staging = Path(tempfile.mkdtemp(prefix="toolchain-", dir=bootstrap))
    try:
        safe_extract(cached_archive, staging)
        executable = "go.exe" if key.startswith("windows-") else "go"
        candidates = [p.parent.parent for p in staging.rglob(f"bin/{executable}") if p.is_file()]
        if len(candidates) != 1:
            raise BuildError(f"Tailscale Go archive must contain exactly one bin/{executable}")
        extracted = candidates[0]
        version_file = extracted / "VERSION"
        if not version_file.is_file() or version_file.read_text(encoding="utf-8").splitlines()[0] != lock["version"]:
            raise BuildError("Tailscale Go archive VERSION does not match lock")
        goroot = bootstrap / f"tailscale-go-{key}"
        if goroot.exists() or goroot.is_symlink():
            if goroot.is_symlink():
                raise BuildError("Toolchain installation must not be a symlink")
            shutil.rmtree(goroot)
        if extracted == staging:
            staging.rename(goroot)
        else:
            extracted.rename(goroot)
    finally:
        if staging.exists():
            shutil.rmtree(staging)
    env = go_environment(goroot, cache)
    go = goroot / "bin" / executable
    if Path(shutil.which(executable, path=env["PATH"]) or "").resolve() != go.resolve():
        raise BuildError("PATH did not select the verified Tailscale Go binary")
    identity = json.loads(run([go, "env", "-json", "GOROOT", "GOTOOLCHAIN", "GOVERSION"], env=env, capture=True))
    if (Path(identity["GOROOT"]).resolve() != goroot.resolve()
            or identity["GOTOOLCHAIN"] != "local" or identity["GOVERSION"] != lock["version"]):
        raise BuildError("Running Go identity differs from the verified toolchain")
    provenance.update({"revision": pins["tailscale_go_revision"], "version": lock["version"],
                       "host": key, "checksum_source": lock["checksum_source"],
                       "binary_sha256": sha256(go), "selection": "explicit-GOROOT/PATH; GOTOOLCHAIN=local"})
    write_json(bootstrap / "toolchain-provenance.json", provenance)
    return go, env, provenance


def source_checkout(pins: dict, bootstrap: Path) -> Path:
    source = bootstrap / "sing-box"
    git_env = dict(os.environ)
    git_env.update({"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                    "GIT_TERMINAL_PROMPT": "0"})
    # Ambient git variables must not redirect the repo/index/object store.
    git_env = {k: v for k, v in git_env.items() if not k.startswith("GIT_") or k in
               ("GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_TERMINAL_PROMPT")}
    def git(*args: str, capture: bool = True) -> str:
        return run(["git", "-c", f"core.hooksPath={os.devnull}", "-C", source, *args],
                   env=git_env, capture=capture)
    if source.is_symlink():
        raise BuildError("Core checkout must not be a symlink")
    if not source.exists():
        source.mkdir(parents=True)
        git("init")
        git("remote", "add", "origin", CORE_REPO)
        git("fetch", "--depth=1", "origin", f"refs/tags/v{pins['sing_box_version']}:refs/tags/v{pins['sing_box_version']}")
        fetched = git("rev-parse", f"v{pins['sing_box_version']}^{{commit}}")
        if fetched != pins["sing_box_revision"]:
            raise BuildError("Upstream release tag does not resolve to the pinned core commit")
        git("checkout", "--detach", pins["sing_box_revision"])
    if (git("remote", "get-url", "origin") != CORE_REPO
            or git("rev-parse", "HEAD") != pins["sing_box_revision"]
            or git("rev-parse", f"v{pins['sing_box_version']}^{{commit}}") != pins["sing_box_revision"]):
        raise BuildError("Cached source git identity differs from core pins")
    git("fsck", "--no-dangling")
    if git("describe", "--tags") != "v" + pins["sing_box_version"]:
        raise BuildError("Official linker version would differ from sing_box_version")
    if git("status", "--porcelain", "--untracked-files=no"):
        raise BuildError("Cached core checkout has tracked modifications; remove the checkout and retry")
    # Untracked source/go.work/vendor files could alter compilation. Only the known
    # products of this upstream builder may remain after an earlier build.
    allowed = ("libbox.aar", "libbox-legacy.aar", "libbox-sources.jar",
               "libbox-legacy-sources.jar", "Libbox.xcframework/")
    for entry in git("ls-files", "--others", "--exclude-standard").splitlines():
        if not any(entry == name or (name.endswith("/") and entry.startswith(name)) for name in allowed):
            raise BuildError("Cached core checkout contains unexpected untracked files")
    # Also catch ignored files (notably a vendor tree) that affect Go resolution.
    for entry in git("ls-files", "--others", "--ignored", "--exclude-standard").splitlines():
        if not any(entry == name or (name.endswith("/") and entry.startswith(name)) for name in allowed):
            raise BuildError("Cached core checkout contains unexpected ignored files")
    module = (source / "go.mod").read_text(encoding="utf-8")
    if not re.search(rf"(?m)^\s*{re.escape(GOMOBILE_MODULE)}\s+{re.escape(pins['gomobile_version'])}\s*$", module):
        raise BuildError("Core go.mod gomobile version differs from native-core.json")
    builder = (source / "cmd/internal/build_libbox/main.go").read_text(encoding="utf-8")
    # Inspect active code, not obsolete commented-out flags.
    active = "\n".join(line.split("//", 1)[0] for line in builder.splitlines())
    for tag in UPSTREAM_REQUIRED_TAGS:
        if f'"{tag}"' not in active:
            raise BuildError(f"Upstream build_libbox no longer enables required tag {tag}")
    if '"-tags-not-macos=with_low_memory"' not in active:
        raise BuildError("Upstream Apple builder lost its low memory option")
    return source


def mobile_profile_bytes(original: bytes, profile: dict) -> bytes:
    if hashlib.sha256(original).hexdigest() != profile["upstream_builder_sha256"]:
        raise BuildError("Upstream builder hash differs from the reviewed mobile feature profile")
    text = original.decode()
    pattern = r'sharedTags = append\(sharedTags, [^\n]+\)'
    candidates = [match for match in re.finditer(pattern, text)
                  if all(f'"{tag}"' in match[0] for tag in profile["drop_tags"])]
    if len(candidates) != 1:
        raise BuildError("Mobile profile sharedTags replacement is ambiguous")
    match = candidates[0]
    replacement = match[0]
    for tag in profile["drop_tags"]:
        if tag in REQUIRED_TAGS or not re.fullmatch(r"with_[a-z_]+", tag):
            raise BuildError("Mobile profile attempts to remove a required or invalid tag")
        needle = f', "{tag}"'
        if replacement.count(needle) != 1:
            raise BuildError("Mobile profile tag replacement is ambiguous")
        replacement = replacement.replace(needle, "", 1)
    return (text[:match.start()] + replacement + text[match.end():]).encode()


@contextlib.contextmanager
def mobile_profile(source: Path):
    """A single hashed, reversible edit to the official builder's feature list."""
    profile_path = ROOT / "tool/mobile-profile.json"
    profile = json.loads(profile_path.read_text(encoding="utf-8"))
    builder = source / "cmd/internal/build_libbox/main.go"
    original = builder.read_bytes()
    patched = mobile_profile_bytes(original, profile)
    provenance = {"name": profile["name"], "profile_sha256": sha256(profile_path),
                  "upstream_builder_sha256": hashlib.sha256(original).hexdigest(),
                  "patched_builder_sha256": hashlib.sha256(patched).hexdigest(),
                  "removed_tags": profile["drop_tags"]}
    try:
        builder.write_bytes(patched)
        yield provenance
    finally:
        builder.write_bytes(original)


def upstream_tags(source: Path, target: str) -> list[str]:
    text = (source / "cmd/internal/build_libbox/main.go").read_text(encoding="utf-8")
    active = "\n".join(line.split("//", 1)[0] for line in text.splitlines())
    tags = []
    groups = ["sharedTags", "darwinTags"] if target == "apple" else ["sharedTags"]
    for group in groups:
        for match in re.finditer(rf"{group}\s*=\s*append\({group},([^)]*)\)", active):
            tags.extend(re.findall(r'"([^"\n]+)"', match[1]))
    if target == "apple":
        tags.append("with_low_memory")
    if not set(REQUIRED_TAGS).issubset(tags):
        raise BuildError("Could not determine the official builder feature tags")
    return sorted(set(tags))


def check_sdks(targets: list[str], env: dict) -> dict:
    versions = {}
    if "apple" in targets:
        if platform.system() != "Darwin":
            raise BuildError("Apple builds require macOS with Xcode and iOS simulator/device SDKs")
        for sdk in ("iphoneos", "iphonesimulator"):
            versions[sdk] = run(["xcrun", "--sdk", sdk, "--show-sdk-version"], env=env, capture=True)
        versions["xcode"] = run(["xcodebuild", "-version"], env=env, capture=True).replace("\n", "; ")
    if "macos" in targets:
        if platform.system() != "Darwin":
            raise BuildError("macOS builds require macOS with Xcode and the macOS SDK")
        versions["macosx"] = run(["xcrun", "--sdk", "macosx", "--show-sdk-version"], env=env, capture=True)
        if "xcode" not in versions:
            versions["xcode"] = run(["xcodebuild", "-version"], env=env, capture=True).replace("\n", "; ")
    if "android" in targets:
        candidates = [env.get("ANDROID_HOME"), env.get("ANDROID_SDK_ROOT"),
                      str(Path.home() / "Android/Sdk"), str(Path.home() / ".local/lib/android/sdk"),
                      str(Path.home() / "Library/Android/sdk")]
        sdk = next((Path(p).resolve() for p in candidates if p and
                    (Path(p) / "licenses/android-sdk-license").is_file()), None)
        if sdk is None:
            raise BuildError("Android SDK not found. Set ANDROID_HOME to a licensed SDK; no SDK is installed automatically")
        ndk = sdk / "ndk" / NDK_VERSION
        if not (ndk / "source.properties").is_file():
            raise BuildError(f"Android NDK {NDK_VERSION} is required in ANDROID_HOME/ndk (no global installation performed)")
        if not (sdk / "platforms").is_dir() or not (sdk / "build-tools").is_dir():
            raise BuildError("Android SDK platforms and build-tools are required")
        java_home = env.get("JAVA_HOME")
        java = str(Path(java_home) / "bin/java") if java_home else shutil.which("java", path=env["PATH"])
        if not java:
            raise BuildError("Android builds require OpenJDK 17 (set JAVA_HOME)")
        java_version = run([java, "--version"], env=env, capture=True)
        if "openjdk 17" not in java_version:
            raise BuildError("Pinned upstream build_libbox requires OpenJDK 17")
        env.update({"ANDROID_HOME": str(sdk), "ANDROID_SDK_ROOT": str(sdk),
                    "ANDROID_NDK_HOME": str(ndk), "NDK": str(ndk)})
        versions.update({"android_ndk": NDK_VERSION, "java": java_version.splitlines()[0],
                         "android_platforms": sorted(p.name for p in (sdk / "platforms").iterdir()),
                         "android_build_tools": sorted(p.name for p in (sdk / "build-tools").iterdir())})
    return versions


def network_run(args: list[str | Path], **kwargs) -> str:
    for attempt in range(3):
        try:
            return run(args, **kwargs)
        except subprocess.CalledProcessError:
            if attempt == 2:
                raise
            delay = 2 ** (attempt + 1)
            print(f"Network command failed; retrying in {delay}s with checksum verification enabled", flush=True)
            time.sleep(delay)
    raise AssertionError("unreachable")


def verify_dependency_toolchain(revision: str, current: str, next_revision: str) -> None:
    expected = {current.strip(), next_revision.strip()} - {""}
    if revision not in expected:
        raise BuildError("Pinned Tailscale Go " + revision + " conflicts with the pinned Tailscale dependency's runtime requirement "
                         + ", ".join(sorted(expected)) + ". Resolve the pin conflict explicitly; mismatch overrides are not used")


def resolved_tailscale_dependency(go: Path, env: dict, source: Path, pins: dict) -> dict:
    tailscale = json.loads(network_run([go, "mod", "download", "-json", "github.com/sagernet/tailscale"],
                                       cwd=source, env=env, capture=True))
    dependency_dir = Path(tailscale["Dir"])
    current = (dependency_dir / "go.toolchain.rev").read_text(encoding="utf-8").strip()
    next_revision = (dependency_dir / "go.toolchain.next.rev").read_text(encoding="utf-8").strip()
    verify_dependency_toolchain(pins["tailscale_go_revision"], current, next_revision)
    return {"module": tailscale["Path"], "version": tailscale["Version"],
            "sum": tailscale["Sum"], "go_mod_sum": tailscale["GoModSum"],
            "toolchain_revision": current, "next_toolchain_revision": next_revision}


def prepare_dependencies(go: Path, env: dict, source: Path, pins: dict, *,
                         packages: list[str] | None = None, tags: list[str] | None = None) -> dict:
    """Resolve the requested graph and verify cached sources before compilation."""
    if packages is None:
        network_run([go, "mod", "download"], cwd=source, env=env)
    else:
        # Resolve only the selected target's imports, including its tests. A bare
        # mod download fetches every required module, including unused Cronet ZIPs.
        # Keep the authenticated module locks read-only; this never compiles code.
        network_run([go, "list", "-mod=readonly", "-deps", "-test", "-tags", ",".join(tags or []),
                     *packages], cwd=source, env=env, capture=True)
    # Even a minimal core retains the explicit Tailscale compiler contract. Resolve
    # it before the final verification so this download is checked as well.
    dependency = resolved_tailscale_dependency(go, env, source, pins)
    run([go, "mod", "verify"], cwd=source, env=env)
    return dependency


def install_mobile(go: Path, env: dict, source: Path, pins: dict) -> tuple[dict, dict]:
    # Stay in the verified core module. An @version go install outside the module
    # queries unrelated latest versions for deprecation notices; this uses go.mod.
    modules = json.loads(network_run([go, "mod", "download", "-json", GOMOBILE_MODULE],
                                     cwd=source, env=env, capture=True))
    if modules.get("Version") != pins["gomobile_version"]:
        raise BuildError("Selected gomobile module differs from native-core.json")
    # Go authenticates downloads against go.sum / sum.golang.org; also verify
    # cached sources and the dependency runtime contract before compiling tools.
    dependency = prepare_dependencies(go, env, source, pins)
    # Rebuild rather than executing stale or altered cached mobile tools.
    network_run([go, "install", "-trimpath", f"{GOMOBILE_MODULE}/cmd/gomobile",
                 f"{GOMOBILE_MODULE}/cmd/gobind"], cwd=source, env=env)
    binaries = {}
    for name in ("gomobile", "gobind"):
        binary = Path(env["GOBIN"]) / (name + (".exe" if os.name == "nt" else ""))
        info = run([go, "version", "-m", binary], env=env, capture=True)
        if (f"\tmod\t{GOMOBILE_MODULE}\t{pins['gomobile_version']}\t" not in info
                or f"\tpath\t{GOMOBILE_MODULE}/cmd/{name}" not in info):
            raise BuildError(f"{name} binary identity does not match the pinned module")
        binaries[name] = sha256(binary)
    return ({"module": GOMOBILE_MODULE, "version": pins["gomobile_version"],
             "sum": modules["Sum"], "go_mod_sum": modules["GoModSum"], "binaries": binaries}, dependency)


def retain_wgc_license(go: Path, env: dict, source: Path, cache: Path, pins: dict) -> Path:
    module = json.loads(run([go, "list", "-m", "-json", "github.com/idy/wgc"],
                            cwd=source, env=env, capture=True))
    if module.get("Version") != pins["wgc_version"]:
        raise BuildError("Resolved WGC module does not match native-core.json")
    license_path = Path(module["Dir"]) / "LICENSE"
    if license_path.is_symlink() or not license_path.is_file():
        raise BuildError("Pinned WGC module has no regular LICENSE file")
    destination = cache / "results/LICENSE.wgc"
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(license_path, destination)
    return destination


def package_target(target: str, pins: dict, cache: Path) -> Path:
    metadata_path = cache / "results" / f"{target}.json"
    if not metadata_path.is_file():
        raise BuildError(f"No verified {target} build metadata; run a build first")
    metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
    validate_metadata(metadata, pins, target, allow_custom=True)
    artifact = ROOT / ARTIFACTS[target]
    if metadata["artifact_sha256"] != artifact_hashes(artifact):
        raise BuildError("Artifact differs from the recorded build; rebuild before packaging")
    license_path = cache / "results/LICENSE.sing-box"
    if not license_path.is_file() or sha256(license_path) != metadata.get("license_sha256"):
        raise BuildError("Missing or changed upstream license; rebuild before packaging")
    release = ROOT / ".build/release"
    release.mkdir(parents=True, exist_ok=True)
    destination = release / archive_name(pins, target)
    # Fixed timestamps/owners/order for a repeatable container; SDK output itself
    # is not claimed to be bit-identical across different Xcode/NDK versions.
    def normalize(info: tarfile.TarInfo) -> tarfile.TarInfo:
        info.uid = info.gid = info.mtime = 0
        info.uname = info.gname = ""
        return info
    with destination.open("wb") as raw, gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
            paths = [artifact, *sorted(artifact.rglob("*"))] if artifact.is_dir() else [artifact]
            for path in paths:
                archive.add(path, arcname=path.relative_to(ROOT).as_posix(), recursive=False, filter=normalize)
            archive.add(metadata_path, arcname="native-build.json", filter=normalize)
            archive.add(license_path, arcname="LICENSE.sing-box", filter=normalize)
            legal = legal_files(wgc_license=cache / "results/LICENSE.wgc")
            if {name: sha256(path) for name, path in legal.items()} != metadata.get("legal_sha256"):
                raise BuildError("Legal notices changed since the native build")
            for name, path in sorted(legal.items()):
                archive.add(path, arcname="Legal/" + name, recursive=False, filter=normalize)
    return destination


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--target", choices=("android", "apple", "all"), default="all")
    result.add_argument("--dry-run", action="store_true", help="Show pins, environment and commands without downloads/writes")
    result.add_argument("--bootstrap-dir", default=str(ROOT / ".build/bootstrap"))
    result.add_argument("--cache-dir", default=str(ROOT / ".build/cache"))
    result.add_argument("--bootstrap-only", action="store_true", help="Verify/download fork and core; install pinned mobile tools")
    result.add_argument("--package-only", action="store_true", help="Package existing verified artifacts for a future release")
    result.add_argument("--wgc-config", type=Path, help="JSON manifest of compiled WGC transport extensions")
    return result


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        pins = load_pins()
        extensions = load_extensions(args.wgc_config)
        bootstrap = build_directory(args.bootstrap_dir)
        cache = build_directory(args.cache_dir)
        if bootstrap == cache or bootstrap.is_relative_to(cache) or cache.is_relative_to(bootstrap):
            raise BuildError("Bootstrap and cache directories must be separate, non-nested .build directories")
        targets = ["android", "apple"] if args.target == "all" else [args.target]
        if args.bootstrap_only and args.package_only:
            raise BuildError("Choose either --bootstrap-only or --package-only")
        if args.dry_run:
            print(json.dumps({"pins": pin_identity(pins), "host": host_key(),
                              "bootstrap_dir": str(bootstrap.relative_to(ROOT)), "cache_dir": str(cache.relative_to(ROOT)),
                              "environment": {"GOTOOLCHAIN": "local", "GOROOT": "<verified fork>", "PATH": "<verified fork>/bin:<project GOPATH>/bin:..."},
                              "tags": list(REQUIRED_TAGS), "apple_extra_tags": ["with_low_memory", "with_dhcp", "grpcnotrace"],
                              "commands": ["<fork>/bin/go install github.com/sagernet/gomobile/cmd/{gomobile,gobind} (in verified core module; pinned " + pins["gomobile_version"] + ")",
                                           *["<fork>/bin/go run ./cmd/internal/build_libbox -target " + target + (" -platform ios,iossimulator" if target == "apple" else "") for target in targets]],
                              "outputs": [ARTIFACTS[target] for target in targets],
                              "mode": "package" if args.package_only else "bootstrap" if args.bootstrap_only else "build"}, indent=2))
            return 0
        with project_lock():
            cache.mkdir(parents=True, exist_ok=True)
            bootstrap.mkdir(parents=True, exist_ok=True)
            if not args.package_only:
                # Fail early for missing SDKs, without downloading 60MB of toolchain.
                sdk_env = dict(os.environ)
                sdk_versions = {} if args.bootstrap_only else check_sdks(targets, sdk_env)
                go, env, toolchain = bootstrap_go(pins, bootstrap, cache)
                for key in ("ANDROID_HOME", "ANDROID_SDK_ROOT", "ANDROID_NDK_HOME", "NDK"):
                    if key in sdk_env:
                        env[key] = sdk_env[key]
                source = source_checkout(pins, bootstrap)
                notices = cache / "results/LICENSE.sing-box"
                notices.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(source / "LICENSE", notices)
                mobile, tailscale_dependency = install_mobile(go, env, source, pins)
                if args.bootstrap_only:
                    print("Verified Tailscale Go, source and gomobile bootstrap complete.")
                    return 0
                for target in targets:
                    args_go = [go, "run", "./cmd/internal/build_libbox", "-target", target]
                    if target == "apple":
                        args_go += ["-platform", "ios,iossimulator"]
                    output = source / ("libbox.aar" if target == "android" else "Libbox.xcframework")
                    if output.exists():
                        shutil.rmtree(output) if output.is_dir() else output.unlink()
                    with mobile_profile(source) as profile_provenance, wgc_overlay(source, go, env, run, extensions) as wgc_provenance:
                        wgc_license = retain_wgc_license(go, env, source, cache, pins)
                        tailscale_dependency = resolved_tailscale_dependency(go, env, source, pins)
                        effective_tags = upstream_tags(source, target)
                        run(args_go, cwd=source, env=env)
                    if not output.exists():
                        raise BuildError(f"Official builder completed without {output.name}")
                    destination = ROOT / ARTIFACTS[target]
                    destination.parent.mkdir(parents=True, exist_ok=True)
                    if destination.exists():
                        shutil.rmtree(destination) if destination.is_dir() else destination.unlink()
                    if output.is_dir():
                        shutil.copytree(output, destination, symlinks=True)
                    else:
                        shutil.copyfile(output, destination)
                    if target == "android":
                        sources_jar = source / "libbox-sources.jar"
                        if sources_jar.is_file():
                            generated = cache / "generated/android"
                            generated.mkdir(parents=True, exist_ok=True)
                            shutil.copyfile(sources_jar, generated / sources_jar.name)
                    metadata = {"schema": 1, "target": target, "pins": pin_identity(pins),
                                "source": {"repository": CORE_REPO, "tag": "v" + pins["sing_box_version"],
                                           "revision": pins["sing_box_revision"], "go_sum_sha256": sha256(source / "go.sum"),
                                           "linker_version": pins["sing_box_version"], "linker_version_source": "verified exact git describe tag"},
                                "toolchain": toolchain, "gomobile": mobile, "tailscale_dependency": tailscale_dependency, "sdks": sdk_versions,
                                "tags": effective_tags, "mobile_profile": profile_provenance,
                                "wgc": wgc_provenance,
                                "legal_sha256": {name: sha256(path) for name, path in legal_files(wgc_license=wgc_license).items()},
                                "platform": "ios,iossimulator" if target == "apple" else "android",
                                "artifact_sha256": artifact_hashes(destination), "license_sha256": sha256(notices)}
                    write_json(cache / "results" / f"{target}.json", metadata)
            archives = [package_target(target, pins, cache) for target in targets]
            sums = "".join(f"{sha256(path)}  {path.name}\n" for path in archives)
            (ROOT / ".build/release/SHA256SUMS").write_text(sums, encoding="utf-8")
            print("Native artifacts and SHA256SUMS are ready in .build/release.")
        return 0
    except (BuildError, OSError, ValueError, KeyError, tarfile.TarError, subprocess.CalledProcessError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
