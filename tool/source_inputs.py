"""Source fingerprints and the canonical inputs for the bundled WGC adapter."""
from __future__ import annotations

import hashlib
import os
from pathlib import Path

from native_common import BuildError

SOURCE_IGNORES = frozenset({".git", ".hg", ".svn", ".build", ".wgc-inputs"})

ADAPTER_INPUTS = frozenset({
    "endpoint.go", "endpoint_test.go", "rebind.go", "rebind_test.go",
    "transport.go", "go.mod", "go.sum",
})


def source_digest(root: Path) -> str:
    """Conservatively hash module contents, including assembly and embedded data."""
    if not (root / "go.mod").is_file():
        raise BuildError("WGC source module is missing")
    files = []
    ignored = SOURCE_IGNORES
    for directory, folders, names in os.walk(root):
        folders[:] = sorted(name for name in folders if name not in ignored)
        for name in [*folders, *names]:
            path = Path(directory) / name
            if name not in ignored and path.is_symlink():
                raise BuildError("WGC source inputs must not be symlinks")
        files.extend(Path(directory) / name for name in names
                     if name not in ignored and (Path(directory) / name).is_file())
    files.sort()
    digest = hashlib.sha256()
    for path in files:
        if path.is_symlink():
            raise BuildError("WGC source inputs must not be symlinks")
        digest.update(path.relative_to(root).as_posix().encode() + b"\0")
        digest.update(hashlib.sha256(path.read_bytes()).digest())
        digest.update(b"\0")
    return digest.hexdigest()


def adapter_digest(root: Path) -> str:
    """Reject adapter inputs that cannot match the canonical published module."""
    if root.is_symlink():
        raise BuildError("WGC adapter source directory must not be a symlink")
    if not root.is_dir():
        raise BuildError("WGC adapter source directory is missing")
    inputs = {path.name: path for path in root.iterdir()
              if path.name not in {".git", ".hg", ".svn"}}
    missing = ADAPTER_INPUTS - inputs.keys()
    unexpected = inputs.keys() - ADAPTER_INPUTS
    if missing or unexpected:
        details = []
        if missing:
            details.append("missing: " + ", ".join(sorted(missing)))
        if unexpected:
            details.append("unexpected: " + ", ".join(sorted(unexpected)))
        raise BuildError("WGC adapter input set differs from canonical inputs (" + "; ".join(details) + ")")
    for name, path in sorted(inputs.items()):
        if path.is_symlink() or not path.is_file():
            raise BuildError(f"WGC adapter input must be a regular file, not a symlink: {name}")
    return source_digest(root)
