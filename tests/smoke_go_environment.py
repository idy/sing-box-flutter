#!/usr/bin/env python3
"""Opt-in real pinned-Go delimiter smoke; uses a cached archive without downloads."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
from build_libbox import bootstrap_go, host_key, run
from native_common import BuildError, ROOT, load_pins


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, help="Already cached pinned toolchain archive")
    args = parser.parse_args()
    pins = load_pins()
    archive_name = f"tailscale-go-{pins['tailscale_go_revision']}-{host_key()}.tar.gz"
    archive = args.archive or ROOT / ".build/cache/archives" / archive_name
    if not archive.is_file():
        raise BuildError("A cached pinned archive is required; this smoke never downloads")
    work_parent = ROOT / ".build"
    work_parent.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="go-environment-", dir=work_parent) as temporary:
        work = Path(temporary).resolve()
        bootstrap = work / ("compiler" + os.pathsep + "verified")
        cache = work / ("cache" + os.pathsep + "private")
        bootstrap.mkdir()
        archives = cache / "archives"
        archives.mkdir(parents=True)
        shutil.copyfile(archive, archives / archive_name)
        # Re-extract and verify in isolation; never execute the shared installation.
        go, env, provenance = bootstrap_go(pins, bootstrap, cache)
        env = {**env, "CGO_ENABLED": "0", "GOPROXY": "off", "PATH": ""}
        fields = ("GOROOT", "GOTOOLCHAIN", "GOVERSION", "GOPATH", "GOBIN", "GOMODCACHE", "GOCACHE")
        identity = json.loads(run([go, "env", "-json", *fields], env=env, capture=True))
        if (not go.is_absolute() or os.pathsep in identity["GOPATH"] or
                any(identity[name] != env[name] for name in fields if name != "GOVERSION")):
            raise BuildError("Compiler environment differs from the generated isolated environment")
        for name in ("GOBIN", "GOMODCACHE", "GOCACHE"):
            if not Path(identity[name]).is_relative_to(cache):
                raise BuildError("Compiler caches escaped the isolated work directory")
        source = work / "own-source"
        source.mkdir()
        (source / "go.mod").write_text("module example.invalid/go-environment-smoke\n\ngo 1.24.0\n", encoding="utf-8")
        (source / "main.go").write_text('package main\n\nfunc main() { println("pinned-go-delimiter-ok") }\n', encoding="utf-8")
        output = work / ("smoke.exe" if os.name == "nt" else "smoke")
        run([go, "build", "-o", output, "."], cwd=source, env=env)
        result = subprocess.run([str(output)], cwd=source, env=env, check=True,
                                capture_output=True, text=True, encoding="utf-8")
        if result.stderr.strip() != "pinned-go-delimiter-ok":
            raise BuildError("Compiled smoke returned unexpected output")
        print(json.dumps({"toolchain": provenance, "environment": identity, "path": env["PATH"],
                          "result": result.stderr.strip()}, indent=2))


if __name__ == "__main__":
    main()
