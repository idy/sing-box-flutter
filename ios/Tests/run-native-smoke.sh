#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
set -euo pipefail

if [[ $# -gt 3 ]]; then
  printf 'Usage: %s [booted-simulator-uuid [packet-tunnel-executable [app-executable]]]\n' "$0" >&2
  printf 'No arguments runs Go-free preflight tests. SINGBOX_HOST_PROJECT selects project artifacts.\n' >&2
  exit 64
fi

script_directory="$(dirname "$(realpath "$0")")"
ios_directory="$(dirname "$script_directory")"
package_directory="$(dirname "$ios_directory")"
smoke_directory="$(mktemp -d "${TMPDIR:-/tmp}/sing-box-ios-smoke.XXXXXX")"
trap 'rm -rf "$smoke_directory"' EXIT

printf 'Build and run Foundation-only preflight tests without Go or Flutter.\n'
xcrun swiftc -swift-version 5 -parse-as-library \
  "$ios_directory/Classes/Shared/SharedTypes.swift" \
  "$ios_directory/Classes/Shared/CoreCapabilities.swift" \
  "$ios_directory/Classes/Shared/ConfigPreflight.swift" \
  "$script_directory/PreflightSmoke.swift" -o "$smoke_directory/PreflightSmoke"
"$smoke_directory/PreflightSmoke" "$ios_directory/Resources/core-capabilities.json"
device_sdk="$(xcrun --sdk iphoneos --show-sdk-path)"
xcrun --sdk iphoneos swiftc -typecheck -swift-version 5 -application-extension \
  -target arm64-apple-ios15.0 -sdk "$device_sdk" "$ios_directory"/Classes/Shared/*.swift
if [[ $# -eq 0 ]]; then
  printf 'Shared checks passed. No native artifact was linked or executed.\n'
  exit 0
fi

host_project="${SINGBOX_HOST_PROJECT:-}"
if [[ -z "$host_project" && -f "$package_directory/example/.dart_tool/sing_box_flutter/native.json" ]]; then
  host_project="$package_directory/example"
fi
# Resolve and validate inputs before compiling; never silently substitute legacy
# binaries for a supplied project. Printed paths contain only local build metadata.
python3 -I - "$ios_directory" "$host_project" > "$smoke_directory/inputs" <<'PY'
import hashlib, json, pathlib, plistlib, sys
try:
    ios = pathlib.Path(sys.argv[1])
    sys.path.insert(0, str(ios.parent / 'tool'))
    from native_common import artifact_hashes
    from build_core import host_path
    from core_recipe import verify_current_recipe
    host = pathlib.Path(sys.argv[2]) if sys.argv[2] else None
    if host:
        if not host.is_absolute():
            raise ValueError()
        host = host.resolve(strict=True)
        receipt = json.loads(host_path(host, '.dart_tool/sing_box_flutter/native.json').read_text())
        if receipt['schema'] != 1 or receipt['pubspec_sha256'] != hashlib.sha256((host / 'pubspec.yaml').read_bytes()).hexdigest():
            raise ValueError()
        apple = receipt['artifacts']['apple']
        def resolve(value):
            if not isinstance(value, str) or pathlib.Path(value).is_absolute() or '\\' in value:
                raise ValueError()
            parts = value.split('/')
            if parts[:2] != ['.dart_tool', 'sing_box_flutter'] or any(part in ['', '.', '..'] for part in parts):
                raise ValueError()
            return host_path(host, value).resolve(strict=True)
        framework, capabilities = resolve(apple['path']), resolve(apple['capabilities'])
        hashes = artifact_hashes(framework)
        metadata = json.loads(resolve(apple['metadata']).read_text())
        verify_current_recipe(host, receipt, metadata, target='apple')
        if not hashes or apple['sha256'] != hashes:
            raise ValueError()
        manifest = json.loads(capabilities.read_text())
        if any(manifest[key] != receipt[key] for key in ['schema', 'version', 'fingerprint', 'features']):
            raise ValueError()
        if apple['capabilities_sha256'] != hashlib.sha256(capabilities.read_bytes()).hexdigest():
            raise ValueError()
    else:
        framework, capabilities = ios / 'Frameworks/Libbox.xcframework', ios / 'Resources/core-capabilities.json'
    libraries = plistlib.loads((framework / 'Info.plist').read_bytes())['AvailableLibraries']
    paths = []
    for variant in [None, 'simulator']:
        matches = [lib for lib in libraries if lib['SupportedPlatform'] == 'ios' and lib.get('SupportedPlatformVariant') == variant]
        if len(matches) != 1:
            raise ValueError()
        lib = matches[0]
        path = (framework / lib['LibraryIdentifier'] / lib['LibraryPath']).resolve(strict=True)
        if not path.is_relative_to(framework.resolve()) or not (path / 'Libbox').is_file():
            raise ValueError()
        paths.append(str(path.parent))
    print('\n'.join([*paths, str(capabilities), 'selected' if host else 'legacy']))
except Exception:
    sys.exit('Invalid or missing native smoke artifacts; build the requested host core first.')
PY
{
  IFS= read -r device_framework
  IFS= read -r simulator_framework
  IFS= read -r capabilities_path
  IFS= read -r core_kind
} < "$smoke_directory/inputs"
swift_flags=()
if [[ "$core_kind" == selected ]]; then swift_flags=(-D SINGBOX_SELECTABLE_CORE); fi
simulator_sdk="$(xcrun --sdk iphonesimulator --show-sdk-path)"
native_link_flags=(
  -framework Libbox -framework NetworkExtension -framework Network
  -framework Security -framework SystemConfiguration -framework CoreFoundation
  -framework UIKit -lresolv
)

printf 'Compile and link the selected extension for physical arm64, iOS 15.\n'
xcrun --sdk iphoneos swiftc -emit-library -module-name sing_box_flutter \
  -swift-version 5 -application-extension -target arm64-apple-ios15.0 \
  -sdk "$device_sdk" -F "$device_framework" ${swift_flags[@]+"${swift_flags[@]}"} \
  "${native_link_flags[@]}" "$ios_directory"/Classes/Shared/*.swift \
  "$ios_directory"/Classes/PacketTunnel/*.swift -o "$smoke_directory/PacketTunnel.dylib"

simulator_architecture="$(uname -m)"
case "$simulator_architecture" in
  arm64|x86_64) ;;
  *) printf 'Unsupported simulator host architecture.\n' >&2; exit 1 ;;
esac
printf 'Build and execute native runtime contract checks in the supplied simulator.\n'
xcrun --sdk iphonesimulator swiftc -swift-version 5 \
  -target "$simulator_architecture-apple-ios15.0-simulator" \
  -sdk "$simulator_sdk" -F "$simulator_framework" ${swift_flags[@]+"${swift_flags[@]}"} \
  "${native_link_flags[@]}" "$ios_directory"/Classes/Shared/*.swift \
  "$ios_directory/Classes/PacketTunnel/NativeCore.swift" \
  "$script_directory/NativeCoreSmoke.swift" -o "$smoke_directory/NativeCoreSmoke"
unset TS_PERMIT_TOOLCHAIN_MISMATCH SIMCTL_CHILD_TS_PERMIT_TOOLCHAIN_MISMATCH
xcrun simctl spawn "$1" "$smoke_directory/NativeCoreSmoke" "$capabilities_path"

if [[ $# -ge 2 ]]; then
  printf 'Check that the embedded extension does not link Flutter.\n'
  linked_dependencies="$(xcrun otool -L "$2")"
  if [[ "$linked_dependencies" == *Flutter.framework* || "$linked_dependencies" == *Flutter/Flutter* ]]; then
    printf 'The Packet Tunnel extension unexpectedly links Flutter.\n' >&2
    exit 1
  fi
fi
if [[ $# -eq 3 ]]; then
  printf 'Check that the main app has no Go/libbox linkage or runtime symbols.\n'
  xcrun nm "$3" > "$smoke_directory/app-symbols"
  xcrun otool -L "$3" >> "$smoke_directory/app-symbols"
  if grep -Eq 'Libbox|_crosscall2|_runtime\.rt0_go|_runtime\.main|_go_seq_' "$smoke_directory/app-symbols"; then
    printf 'The main app unexpectedly contains the Go core.\n' >&2
    exit 1
  fi
fi
printf 'Native smoke checks passed. Device VPN behavior still requires signed-device testing.\n'
