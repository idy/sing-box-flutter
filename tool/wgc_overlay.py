"""Reversible, source-identified WGC registration in the pinned upstream libbox."""
from __future__ import annotations

from collections.abc import Callable
import contextlib
import json
from pathlib import Path
import re
import shutil

from native_common import BuildError, ROOT, sha256
from source_inputs import SOURCE_IGNORES, adapter_digest, source_digest

MODULE = "github.com/idy/sing-box-flutter/native/wgc"
REGISTRY_SHA256 = "cbc7bc06391fe555828782df7bc1ce0844e446a82647448739dbb3e8e3eef19e"
IMPORT_PATH = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]+$")
VERSION = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+[A-Za-z0-9.+-]*$")
# A SHA-256 digest encodes as 43 Base64 characters with two zero padding bits.
CHECKSUM = re.compile(r"^h1:[A-Za-z0-9+/]{42}[AEIMQUYcgkosw048]=$")


def extension_checksums(entry: dict) -> dict[str, str]:
    locked = {entry["version"]: entry.get("sum"), entry["version"] + "/go.mod": entry.get("go_mod_sum")}
    if not all(isinstance(value, str) and CHECKSUM.fullmatch(value) for value in locked.values()):
        raise BuildError("Remote WGC extensions require pinned module and go.mod checksums")
    return locked


def snapshot_module(source: Path, destination: Path, expected: str) -> Path:
    shutil.copytree(source, destination, symlinks=True, ignore=shutil.ignore_patterns(*SOURCE_IGNORES))
    if source_digest(destination) != expected:
        raise BuildError("Local WGC source changed before its snapshot was captured")
    return destination


def load_extensions(config: Path | None) -> list[dict]:
    if config is None:
        return []
    config = config.expanduser().resolve(strict=True)
    document = json.loads(config.read_text(encoding="utf-8"))
    if not isinstance(document, dict) or set(document) != {"extensions"} or not isinstance(document["extensions"], list):
        raise BuildError("WGC build config must contain an extensions list")
    extensions = []
    packages = set()
    modules = {}
    for item in document["extensions"]:
        if not isinstance(item, dict) or set(item) - {"package", "module", "version", "path", "sum", "go_mod_sum"}:
            raise BuildError("Invalid WGC extension entry")
        package, module, version = item.get("package"), item.get("module"), item.get("version", "v0.0.0")
        if "version" not in item and isinstance(module, str):
            pattern = r"\.v([0-9]+)(?:-unstable)?$" if module.startswith("gopkg.in/") else r"/v([1-9][0-9]*)$"
            suffix = re.search(pattern, module)
            if suffix:
                version = f"v{suffix[1]}.0.0"
        if (not all(isinstance(v, str) for v in (package, module, version)) or
                not IMPORT_PATH.fullmatch(package) or not IMPORT_PATH.fullmatch(module) or
                ".." in package or ".." in module or not VERSION.fullmatch(version) or
                not (package == module or package.startswith(module + "/")) or
                module in (MODULE, "github.com/sagernet/sing-box", "github.com/idy/wgc") or
                package in packages):
            raise BuildError("Invalid, duplicate, or reserved WGC extension module")
        entry = {"package": package, "module": module, "version": version}
        if "path" in item:
            if "sum" in item or "go_mod_sum" in item:
                raise BuildError("Local WGC extensions use source fingerprints instead of remote checksums")
            if not isinstance(item["path"], str) or not item["path"]:
                raise BuildError("WGC extension path must be a nonempty string")
            local = (config.parent / item["path"]).resolve(strict=True)
            entry.update({"path": str(local), "source_sha256": source_digest(local)})
        else:
            entry.update({"sum": item.get("sum"), "go_mod_sum": item.get("go_mod_sum")})
            extension_checksums(entry)
        identity = (version, entry.get("path"), entry.get("sum"), entry.get("go_mod_sum"), entry.get("source_sha256"))
        if module in modules and modules[module] != identity:
            raise BuildError("Conflicting versions, paths or checksums for a WGC extension module")
        modules[module] = identity
        packages.add(package)
        extensions.append(entry)
    return extensions


def seed_module_checksums(source: Path, module: str, locked: dict[str, str]) -> None:
    sums = source / "go.sum"
    original = sums.read_text(encoding="utf-8")
    present = set()
    for line in original.splitlines():
        fields = line.split()
        if len(fields) == 3 and fields[0] == module and fields[1] in locked:
            if fields[2] != locked[fields[1]]:
                raise BuildError(f"Resolved {module} checksum conflicts with the checked-in lock")
            present.add(fields[1])
    additions = "".join(f"{module} {v} {locked[v]}\n" for v in sorted(locked.keys() - present))
    sums.write_text(original + ("\n" if original and not original.endswith("\n") else "") + additions, encoding="utf-8")


def checksum_entries(text: str) -> dict[tuple[str, str], str]:
    entries = {}
    for line in text.splitlines():
        parts = line.split()
        if len(parts) != 3 or not parts[1].startswith("v") or not parts[2].startswith("h1:"):
            continue
        key, value = (parts[0], parts[1]), parts[2]
        if key in entries and entries[key] != value:
            raise BuildError("Conflicting entries in module checksum lock")
        entries[key] = value
    return entries


def pin_checksum_text(source: Path, text: str, trusted: dict) -> None:
    grouped = {}
    for key, value in checksum_entries(text).items():
        if key in trusted and trusted[key] != value:
            raise BuildError("Dependency checksum conflicts with the authenticated lock")
        trusted[key] = value
        module, version = key
        grouped.setdefault(module, {})[version] = value
    for module, locked in grouped.items():
        seed_module_checksums(source, module, locked)


def seed_wgc_checksums(source: Path, root: Path, module_root: Path | None = None) -> None:
    version = json.loads((root / "native-core.json").read_text(encoding="utf-8"))["wgc_version"]
    wanted = {version, version + "/go.mod"}
    locked = {}
    lock_file = (module_root if module_root is not None else root / "native/wgc") / "go.sum"
    for line in lock_file.read_text(encoding="utf-8").splitlines():
        fields = line.split()
        if len(fields) == 3 and fields[0] == "github.com/idy/wgc" and fields[1] in wanted:
            if not CHECKSUM.fullmatch(fields[2]):
                raise BuildError("Checked-in WGC module and go.mod checksums must use valid h1 SHA-256 hashes")
            locked[fields[1]] = fields[2]
    if set(locked) != wanted:
        raise BuildError("Checked-in WGC module and go.mod checksums are required")
    seed_module_checksums(source, "github.com/idy/wgc", locked)


def core_dependency_modules(source: Path, go: Path, env: dict, run) -> set[str]:
    declared = json.loads(run([go, "mod", "edit", "-json"], cwd=source, env=env, capture=True))
    protected = {entry["Path"] for entry in declared.get("Require", [])}
    protected.update(module for module, _ in checksum_entries((source / "go.sum").read_text(encoding="utf-8")))
    return protected


@contextlib.contextmanager
def wgc_overlay(source: Path, go: Path, env: dict, run, extensions: list[dict], root: Path = ROOT, *,
                resolve_dependencies: Callable[[Path, dict, Path, Callable], object] | None = None):
    """Register WGC and restore upstream inputs on all exits.

    By default tidy resolves the standard/mobile graph. A selected-package
    resolver receives (go, resolution_env, source, run) and must only read package
    graphs, without compiling or executing code. Module updates are enabled only
    in resolution_env; checksum guards and verification still run before yield.
    """
    registry = source / "include/registry.go"
    builder = source / "cmd/internal/build_libbox/main.go"
    generated = source / "include/wgc_flutter.go"
    if generated.exists():
        raise BuildError("Unexpected WGC overlay already exists in upstream checkout")
    paths = [registry, builder, source / "go.mod", source / "go.sum"]
    originals = {path: path.read_bytes() for path in paths}
    if sha256(registry) != REGISTRY_SHA256:
        raise BuildError("Upstream registry differs from the reviewed WGC registration point")
    marker = "\tregisterTailscaleEndpoint(registry)\n"
    registry_text = originals[registry].decode()
    if registry_text.count(marker) != 1:
        raise BuildError("WGC endpoint registration hook is ambiguous")
    builder_text = originals[builder].decode()
    tag_marker = 'sharedTags = append(sharedTags, "with_gvisor"'
    if builder_text.count(tag_marker) != 1:
        raise BuildError("WGC mobile build tag hook is ambiguous")
    imports = [f'\t_ {json.dumps(item["package"])}' for item in extensions]
    generated_text = ('// Code generated by sing-box-flutter; restored after the build.\n'
                      'package include\n\nimport (\n'
                      '\t"github.com/sagernet/sing-box/adapter/endpoint"\n'
                      f'\twgc "{MODULE}"\n' + '\n'.join(imports) + '\n)\n\n'
                      'func registerWGCEndpoint(registry *endpoint.Registry) {\n'
                      '\twgc.RegisterEndpoint(registry)\n}\n')
    provenance = {"module": MODULE, "source_sha256": adapter_digest(root / "native/wgc"),
                  "overlay_sha256": sha256(root / "tool/wgc_overlay.py"),
                  "upstream_registry_sha256": REGISTRY_SHA256,
                  "extensions": [{k: v for k, v in item.items() if k != "path"} for item in extensions]}
    snapshot_root = source / ".wgc-inputs"
    snapshots_created = False
    try:
        snapshot_root.mkdir(mode=0o700)
        snapshots_created = True
        adapter_snapshot = snapshot_module(root / "native/wgc", snapshot_root / "adapter", provenance["source_sha256"])
        prepared = [dict(item) for item in extensions]
        local_snapshots = {}
        for index, item in enumerate(prepared):
            if "path" in item:
                identity = (item["module"], item["version"], item["path"], item["source_sha256"])
                if identity not in local_snapshots:
                    local_snapshots[identity] = str(snapshot_module(Path(item["path"]), snapshot_root / f"extension-{index}", item["source_sha256"]))
                item["path"] = local_snapshots[identity]
        if prepared:
            protected = core_dependency_modules(source, go, env, run)
            protected.update(module for module, _ in checksum_entries((adapter_snapshot / "go.sum").read_text(encoding="utf-8")))
            if any(item["module"] in protected for item in prepared):
                raise BuildError("WGC extensions must not replace or override core dependency modules")
        registry.write_text(registry_text.replace(marker, marker + "\tregisterWGCEndpoint(registry)\n"), encoding="utf-8")
        builder.write_text(builder_text.replace(tag_marker, 'sharedTags = append(sharedTags, "with_wgc")\n\t' + tag_marker), encoding="utf-8")
        generated.write_text(generated_text, encoding="utf-8")
        run([go, "mod", "edit", f"-require={MODULE}@v0.0.0", f"-replace={MODULE}=./{adapter_snapshot.relative_to(source).as_posix()}"], cwd=source, env=env)
        for item in prepared:
            options = [f"-require={item['module']}@{item['version']}"]
            if "path" in item:
                options.append(f"-replace={item['module']}=./{Path(item['path']).relative_to(source).as_posix()}")
            run([go, "mod", "edit", *options], cwd=source, env=env)
        # Go reads only the main module's sums, so promote authenticated module locks.
        trusted = checksum_entries(originals[source / "go.sum"].decode())
        pin_checksum_text(source, (adapter_snapshot / "go.sum").read_text(encoding="utf-8"), trusted)
        seed_wgc_checksums(source, root, adapter_snapshot)
        for item in prepared:
            if "path" not in item:
                locked = extension_checksums(item)
                seed_module_checksums(source, item["module"], locked)
                trusted.update({(item["module"], version): value for version, value in locked.items()})
        for item in prepared:
            if "path" in item:
                module_root = Path(item["path"])
            else:
                downloaded = json.loads(run([go, "mod", "download", "-json", f"{item['module']}@{item['version']}"],
                                            cwd=source, env=env, capture=True))
                if downloaded.get("Sum") != item["sum"] or downloaded.get("GoModSum") != item["go_mod_sum"]:
                    raise BuildError("Downloaded WGC extension does not match its pinned checksums")
                module_root = Path(downloaded["Dir"])
            dependency_sums = module_root / "go.sum"
            if dependency_sums.is_file():
                pin_checksum_text(source, dependency_sums.read_text(encoding="utf-8"), trusted)
        if resolve_dependencies is None:
            tidy_env = dict(env, GOFLAGS="-trimpath -buildvcs=false")
            run([go, "mod", "tidy"], cwd=source, env=tidy_env)
        else:
            resolution_env = dict(env, GOFLAGS="-mod=mod -trimpath -buildvcs=false")
            resolve_dependencies(go, resolution_env, source, run)
        for item in prepared:
            selected = json.loads(run([go, "list", "-m", "-json", item["module"]],
                                      cwd=source, env=env, capture=True))
            if selected.get("Version") != item["version"]:
                raise BuildError(f"WGC extension {item['module']} resolved to {selected.get('Version')}, expected {item['version']}")
            if "path" in item:
                if Path(selected.get("Dir", "")).resolve() != Path(item["path"]).resolve():
                    raise BuildError("Local WGC extension resolved outside its verified snapshot")
            elif (selected.get("Replace") or selected.get("Sum") != item["sum"] or
                    selected.get("GoModSum") != item["go_mod_sum"]):
                raise BuildError("Resolved WGC extension source differs from its pinned module")
        for key, value in checksum_entries((source / "go.sum").read_text(encoding="utf-8")).items():
            if trusted.get(key) != value:
                raise BuildError(f"Unpinned build dependency {key[0]}@{key[1]}; update the module go.sum from a trusted source")
        run([go, "mod", "verify"], cwd=source, env=env)
        provenance["go_sum_sha256"] = sha256(source / "go.sum")
        yield provenance
    finally:
        try:
            for path, original in originals.items():
                path.write_bytes(original)
            generated.unlink(missing_ok=True)
        finally:
            if snapshots_created:
                shutil.rmtree(snapshot_root)
