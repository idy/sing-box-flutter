#!/usr/bin/env python3
"""Build a private example host with Flutter SPM; retain it under .build for inspection.

Requires macOS, Xcode, Flutter, CocoaPods, and Ruby with the xcodeproj gem.
Use RUBY to select that Ruby interpreter. No global Flutter/Xcode settings change.
"""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import plistlib
import re
import shlex
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
EXCLUDED = {
    '.git', '.dart_tool', '.build', 'build', 'Pods', '.symlinks', 'ephemeral',
    'xcuserdata', '.idea', '.gradle', 'node_modules', 'Generated.xcconfig',
    'flutter_export_environment.sh', '.flutter-plugins-dependencies',
    'GeneratedPluginRegistrant.swift', 'GeneratedPluginRegistrant.h',
    'GeneratedPluginRegistrant.m', 'local.properties', 'Podfile.lock',
}


def fixture_pubspec(text: str, root: Path, mode: str) -> str:
    """Bounded edits of the known example; fail rather than alter arbitrary YAML."""
    replacements = [
        (r'(?m)^(  sing_box_flutter:\n    path:) \.\./$',
         lambda match: match[1] + ' ' + json.dumps(str(root))),
        (r'(?m)^(    enable-swift-package-manager:) false$', r'\1 true'),
        (r'(?m)^(sing_box_flutter:\n  macos:\n    mode:) (?:ne|user_tun)$',
         lambda match: match[1] + ' ' + mode),
    ]
    for pattern, replacement in replacements:
        text, count = re.subn(pattern, replacement, text)
        if count != 1:
            raise ValueError('Example pubspec structure changed; update the fixture editor')
    return text


def create_fixture(root: Path, platform: str, mode: str) -> Path:
    if platform not in ('ios', 'macos') or mode not in ('ne', 'user_tun'):
        raise ValueError('Unsupported platform or mode')
    if platform == 'ios' and mode != 'ne':
        raise ValueError('iOS supports only ne mode')
    parent = root / '.build'
    parent.mkdir(exist_ok=True)
    host = Path(tempfile.mkdtemp(prefix=f'apple-spm-{platform}-{mode}-', dir=parent))
    shutil.copytree(root / 'example', host, dirs_exist_ok=True,
                    ignore=lambda directory, names: [name for name in names if name in EXCLUDED])
    pubspec = host / 'pubspec.yaml'
    pubspec.write_text(fixture_pubspec(pubspec.read_text(), root, mode))
    return host


def run(command: list[str], *, cwd: Path, env: dict[str, str], capture: bool = False) -> str:
    print('+ ' + shlex.join(command), flush=True)
    result = subprocess.run(command, cwd=cwd, env=env, check=True, text=True,
                            stdout=subprocess.PIPE if capture else None)
    return result.stdout or ''


def verify_bundle(host: Path, platform: str, mode: str, env: dict[str, str]) -> dict:
    products = host / ('build/ios/iphonesimulator' if platform == 'ios' else 'build/macos/Build/Products/Debug')
    apps = list(products.glob('*.app'))
    if len(apps) != 1:
        raise RuntimeError(f'Expected one built application in {products}, found {len(apps)}')
    app = apps[0]
    plugins = app / ('PlugIns' if platform == 'ios' else 'Contents/PlugIns')
    extensions = list(plugins.glob('*.appex'))
    if len(extensions) != (1 if mode == 'ne' else 0):
        raise RuntimeError(f'Unexpected embedded extension set: {extensions}')

    def executable(bundle: Path) -> Path:
        base = bundle if platform == 'ios' else bundle / 'Contents'
        info = plistlib.loads((base / 'Info.plist').read_bytes())
        return base / ('' if platform == 'ios' else 'MacOS') / info['CFBundleExecutable']

    # Debug builds may put code in a separate debug dylib or framework. Inspect
    # every Mach-O inside each owner, excluding the app's embedded extensions.
    def go_owners(bundle: Path) -> list[str]:
        owners = []
        for path in bundle.rglob('*'):
            if not path.is_file() or (bundle == app and any(ext in path.parents for ext in extensions)):
                continue
            with path.open('rb') as stream:
                magic = stream.read(4)
            if magic not in (b'\xcf\xfa\xed\xfe', b'\xce\xfa\xed\xfe', b'\xca\xfe\xba\xbe', b'\xbe\xba\xfe\xca'):
                continue
            symbols = run(['xcrun', 'nm', '-a', str(path)], cwd=host, env=env, capture=True)
            if re.search(r'(?:_)?(?:runtime[.·]rt0_go|_cgo_topofstack|crosscall2)\b', symbols):
                owners.append(str(path))
        return owners

    app_owners = go_owners(app)
    extension_owners = go_owners(extensions[0]) if extensions else []
    if mode == 'ne' and (app_owners or not extension_owners):
        raise RuntimeError(f'NE Go ownership failed: app={app_owners}, extension={extension_owners}')
    if mode == 'user_tun' and not app_owners:
        raise RuntimeError('user_tun app has no Go runtime symbols')
    return {'app': str(app), 'executable': str(executable(app)),
            'extensions': [str(item) for item in extensions],
            'go_owners': app_owners + extension_owners}


def verify_incremental_guard(host: Path, command: list[str], env: dict[str, str], capabilities: Path) -> None:
    """Unchanged Swift inputs must still reject modified capability bytes."""
    original = capabilities.read_bytes()
    stamp = capabilities.stat()
    if not original:
        raise RuntimeError('Cannot test the guard with an empty capabilities file')
    try:
        capabilities.write_bytes(bytes([original[0] ^ 1]) + original[1:])
        os.utime(capabilities, ns=(stamp.st_atime_ns, stamp.st_mtime_ns))
        # iOS otherwise reduces the build-plugin failure to a generic Xcode
        # summary and hides the diagnostic needed to authenticate this test.
        result = subprocess.run([*command, '--verbose'], cwd=host, env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=600)
        if result.returncode == 0:
            raise RuntimeError('Incremental SPM build accepted modified capabilities')
        if 'Selected Apple Swift package is stale or invalid' not in result.stdout:
            raise RuntimeError('Incremental build failed without the expected SPM guard diagnostic:\n' + result.stdout)
        print('Incremental SPM guard rejected modified bytes with unchanged timestamps.', flush=True)
    finally:
        capabilities.write_bytes(original)
        os.utime(capabilities, ns=(stamp.st_atime_ns, stamp.st_mtime_ns))
    run(command, cwd=host, env=env)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--platform', choices=('ios', 'macos'), required=True)
    parser.add_argument('--mode', choices=('ne', 'user_tun'), default='ne')
    args = parser.parse_args(argv)
    if args.platform == 'ios' and args.mode == 'user_tun':
        parser.error('iOS supports only --mode ne')
    if sys.platform != 'darwin':
        parser.error('Apple builds require macOS and Xcode')
    host = create_fixture(ROOT, args.platform, args.mode)
    print(f'Isolated host (retained even on failure): {host}', flush=True)
    env = {**os.environ, 'FLUTTER_XCODE_CODE_SIGNING_ALLOWED': 'NO'}
    target = 'apple' if args.platform == 'ios' else 'macos'
    run(['flutter', 'pub', 'get'], cwd=host, env=env)
    run(['dart', 'run', 'sing_box_flutter', 'build', '--target', target], cwd=host, env=env)
    native = host / args.platform
    run(['pod', 'deintegrate', 'Runner.xcodeproj'], cwd=native, env=env)
    configure = 'configure_spm.rb' if args.platform == 'ios' else 'configure_project.rb'
    run([env.get('RUBY', 'ruby'), str(native / configure)], cwd=host, env=env)
    command = ['flutter', 'build', args.platform]
    command += ['--simulator', '--no-codesign', '--debug'] if args.platform == 'ios' else ['--debug']
    run(command, cwd=host, env=env)
    # Recheck both the host recipe and actual core bytes after native compilation.
    artifact = json.loads(run([sys.executable, str(ROOT / 'tool/core_recipe.py'),
                              '--project-dir', str(host), '--target', target,
                              '--print-artifact-info'], cwd=host, env=env, capture=True))
    capabilities_path = host / artifact['capabilities']
    capabilities = json.loads(capabilities_path.read_text())
    verify_incremental_guard(host, command, env, capabilities_path)
    report = verify_bundle(host, args.platform, args.mode, env)
    report['incremental_guard'] = 'rejects modified capabilities with unchanged timestamps'
    report.update({'host': str(host), 'platform': args.platform, 'mode': args.mode,
                   'artifact': artifact, 'fingerprint': capabilities['fingerprint'],
                   'capabilities': capabilities})
    (host / 'spm-build-report.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
