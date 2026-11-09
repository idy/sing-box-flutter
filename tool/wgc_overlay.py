"""Reversible, source-identified WGC registration in the pinned upstream libbox."""
from __future__ import annotations

import contextlib
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import tempfile

from native_common import BuildError, ROOT, build_directory, sha256

MODULE = "github.com/idy/sing-box-flutter/native/wgc"
REGISTRY_SHA256 = "cbc7bc06391fe555828782df7bc1ce0844e446a82647448739dbb3e8e3eef19e"
IMPORT_PATH = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]+$")
VERSION = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+[A-Za-z0-9.+-]*$")
CHECKSUM = re.compile(r"^h1:[A-Za-z0-9+/]{43}=$")


def source_digest(root: Path) -> str:
    """Conservatively hash module contents, including assembly and embedded data."""
    if not (root / "go.mod").is_file():
        raise BuildError("WGC source module is missing")
    files = []
    vcs = {".git", ".hg", ".svn"}
    for directory, folders, names in os.walk(root):
        folders[:] = sorted(name for name in folders if name not in vcs)
        for name in [*folders, *names]:
            path = Path(directory) / name
            if name not in vcs and path.is_symlink():
                raise BuildError("WGC source inputs must not be symlinks")
        files.extend(Path(directory) / name for name in names
                     if name not in vcs and (Path(directory) / name).is_file())
    files.sort()
    digest = hashlib.sha256()
    for path in files:
        if path.is_symlink():
            raise BuildError("WGC source inputs must not be symlinks")
        digest.update(path.relative_to(root).as_posix().encode() + b"\0")
        digest.update(hashlib.sha256(path.read_bytes()).digest())
        digest.update(b"\0")
    return digest.hexdigest()


def extension_checksums(entry: dict) -> dict[str, str]:
    locked = {entry["version"]: entry.get("sum"), entry["version"] + "/go.mod": entry.get("go_mod_sum")}
    if not all(isinstance(value, str) and CHECKSUM.fullmatch(value) for value in locked.values()):
        raise BuildError("Remote WGC extensions require pinned module and go.mod checksums")
    return locked


def snapshot_module(source: Path, destination: Path, expected: str) -> Path:
    shutil.copytree(source, destination, symlinks=True, ignore=shutil.ignore_patterns(".git", ".hg", ".svn"))
    if source_digest(destination) != expected:
        raise BuildError("Local WGC source changed before its snapshot was captured")
    return destination


def load_extensions(config: Path | None) -> list[dict]:
    if config is None:
        return []
    config = config.expanduser().resolve(strict=True)
    document = json.loads(config.read_text())
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
        identity = (version, entry.get("path"), entry.get("sum"), entry.get("go_mod_sum"))
        if module in modules and modules[module] != identity:
            raise BuildError("Conflicting versions, paths or checksums for a WGC extension module")
        modules[module] = identity
        packages.add(package)
        extensions.append(entry)
    return extensions


def seed_module_checksums(source: Path, module: str, locked: dict[str, str]) -> None:
    sums = source / "go.sum"
    original = sums.read_text()
    present = set()
    for line in original.splitlines():
        fields = line.split()
        if len(fields) == 3 and fields[0] == module and fields[1] in locked:
            if fields[2] != locked[fields[1]]:
                raise BuildError(f"Resolved {module} checksum conflicts with the checked-in lock")
            present.add(fields[1])
    additions = "".join(f"{module} {v} {locked[v]}\n" for v in sorted(locked.keys() - present))
    sums.write_text(original + ("\n" if original and not original.endswith("\n") else "") + additions)


def seed_wgc_checksums(source: Path, root: Path, module_root: Path | None = None) -> None:
    version = json.loads((root / "native-core.json").read_text())["wgc_version"]
    wanted = {version, version + "/go.mod"}
    locked = {}
    lock_file = (module_root if module_root is not None else root / "native/wgc") / "go.sum"
    for line in lock_file.read_text().splitlines():
        fields = line.split()
        if len(fields) == 3 and fields[0] == "github.com/idy/wgc" and fields[1] in wanted:
            locked[fields[1]] = fields[2]
    if set(locked) != wanted:
        raise BuildError("Checked-in WGC module and go.mod checksums are required")
    seed_module_checksums(source, "github.com/idy/wgc", locked)


@contextlib.contextmanager
def wgc_overlay(source: Path, go: Path, env: dict, run, extensions: list[dict], root: Path = ROOT):
    """Register WGC in the same libbox instance; restore upstream inputs on all exits."""
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
    provenance = {"module": MODULE, "source_sha256": source_digest(root / "native/wgc"),
                  "overlay_sha256": sha256(root / "tool/wgc_overlay.py"),
                  "upstream_registry_sha256": REGISTRY_SHA256,
                  "extensions": [{k: v for k, v in item.items() if k != "path"} for item in extensions]}
    snapshots = None
    try:
        build_root = build_directory(root / ".build", root)
        build_root.mkdir(parents=True, exist_ok=True)
        snapshots = tempfile.TemporaryDirectory(prefix="wgc-inputs-", dir=build_root)
        snapshot_root = Path(snapshots.name)
        adapter_snapshot = snapshot_module(root / "native/wgc", snapshot_root / "adapter", provenance["source_sha256"])
        prepared = [dict(item) for item in extensions]
        for index, item in enumerate(prepared):
            if "path" in item:
                item["path"] = str(snapshot_module(Path(item["path"]), snapshot_root / f"extension-{index}", item["source_sha256"]))
        registry.write_text(registry_text.replace(marker, marker + "\tregisterWGCEndpoint(registry)\n"))
        builder.write_text(builder_text.replace(tag_marker, 'sharedTags = append(sharedTags, "with_wgc")\n\t' + tag_marker))
        generated.write_text(generated_text)
        run([go, "mod", "edit", f"-require={MODULE}@v0.0.0", f"-replace={MODULE}={adapter_snapshot}"], cwd=source, env=env)
        for item in prepared:
            options = [f"-require={item['module']}@{item['version']}"]
            if "path" in item:
                options.append(f"-replace={item['module']}={item['path']}")
            run([go, "mod", "edit", *options], cwd=source, env=env)
        # Go reads the main module's go.sum, including when private scopes skip sumdb.
        seed_wgc_checksums(source, root, adapter_snapshot)
        for item in prepared:
            if "path" not in item:
                seed_module_checksums(source, item["module"], extension_checksums(item))
        tidy_env = dict(env, GOFLAGS="-trimpath -buildvcs=false")
        run([go, "mod", "tidy"], cwd=source, env=tidy_env)
        for item in extensions:
            selected = run([go, "list", "-m", "-f", "{{.Version}}", item["module"]],
                           cwd=source, env=env, capture=True)
            if selected != item["version"]:
                raise BuildError(f"WGC extension {item['module']} resolved to {selected}, expected {item['version']}")
        run([go, "mod", "verify"], cwd=source, env=env)
        provenance["go_sum_sha256"] = sha256(source / "go.sum")
        yield provenance
    finally:
        try:
            for path, original in originals.items():
                path.write_bytes(original)
            generated.unlink(missing_ok=True)
        finally:
            if snapshots is not None:
                snapshots.cleanup()
