#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 || "$1" != /* || ( $# -eq 2 && "$2" != --ci-tun ) ]]; then
  printf 'Usage: %s /absolute/host/project [--ci-tun]\nRequires a built --target macos core including direct, plus Flutter macOS artifacts.\n' "$0" >&2
  exit 64
fi
ci_tun=false
if [[ "${2:-}" == --ci-tun ]]; then
  if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_OS:-}" != macOS || "${SINGBOX_CI_TUN_SMOKE:-}" != 1 ]]; then
    printf 'Real TUN smoke requires GitHub Actions macOS and SINGBOX_CI_TUN_SMOKE=1.\n' >&2
    exit 64
  fi
  if [[ "$EUID" -eq 0 ]]; then
    printf 'Run this harness unprivileged; only its isolated CI traffic executable uses sudo.\n' >&2
    exit 64
  fi
  ci_tun=true
fi
script_directory="$(dirname "$(realpath "$0")")"
macos_directory="$(dirname "$script_directory")"
package_directory="$(dirname "$macos_directory")"
smoke_directory="$(mktemp -d "${TMPDIR:-/tmp}/sing-box-macos-smoke.XXXXXX")"
trap 'rm -rf "$smoke_directory"' EXIT

# Validate the same recipe, tree hashes, capabilities and host-local paths as Pods.
python3 -I - "$package_directory" "$1" "$ci_tun" > "$smoke_directory/inputs" <<'PY'
import json, plistlib, sys
from pathlib import Path
sys.path.insert(0, str(Path(sys.argv[1]) / 'tool'))
from build_core import host_path
from core_recipe import verify_current_recipe, verify_current_artifact
try:
    host = Path(sys.argv[2]).resolve(strict=True)
    receipt = json.loads(host_path(host, '.dart_tool/sing_box_flutter/native.json').read_text())
    if sys.argv[3] == 'true' and receipt.get('macos', {}).get('mode') != 'user_tun':
        raise ValueError()
    artifact = receipt['artifacts']['macos']
    metadata = json.loads(host_path(host, artifact['metadata']).read_text())
    verify_current_recipe(host, receipt, metadata, target='macos')
    verify_current_artifact(host, receipt, 'macos')
    framework = host_path(host, artifact['path']).resolve(strict=True)
    libraries = plistlib.loads((framework / 'Info.plist').read_bytes())['AvailableLibraries']
    slices = [lib for lib in libraries if lib['SupportedPlatform'] == 'macos' and not lib.get('SupportedPlatformVariant')]
    if len(slices) != 1 or not {'arm64', 'x86_64'}.issubset(slices[0]['SupportedArchitectures']):
        raise ValueError()
    slice_path = (framework / slices[0]['LibraryIdentifier'] / slices[0]['LibraryPath']).resolve(strict=True)
    if not slice_path.is_relative_to(framework) or not (slice_path / 'Libbox').is_file():
        raise ValueError()
    print(slice_path.parent)
    print(host_path(host, artifact['capabilities']))
except Exception:
    sys.exit('Invalid macOS native artifacts. Build this host with --target macos first.')
PY
{
  IFS= read -r native_framework
  IFS= read -r capabilities_path
} < "$smoke_directory/inputs"
flutter_executable="$(command -v flutter)"
flutter_directory="${FLUTTER_ROOT:-$(dirname "$(dirname "$(realpath "$flutter_executable")")")}"
flutter_framework="$flutter_directory/bin/cache/artifacts/engine/darwin-x64/FlutterMacOS.xcframework/macos-arm64_x86_64"
if [[ ! -d "$flutter_framework/FlutterMacOS.framework" ]]; then
  printf 'Missing FlutterMacOS framework. Run flutter precache --macos first.\n' >&2
  exit 1
fi
sdk="$(xcrun --sdk macosx --show-sdk-path)"
host_architecture="$(uname -m)"
user_tun_shared_sources=()
for source in "$macos_directory"/Classes/Shared/*.swift; do
  if [[ "$source" != */ConfigPreflight.swift ]]; then
    user_tun_shared_sources+=("$source")
  fi
done

create_capabilities_bundle() {
  # CocoaPods emits a macOS bundle with resources under Contents/Resources.
  # A flat directory would hide packaging bugs in Foundation bundle lookup.
  local bundle="$smoke_directory/sing_box_flutter_capabilities.bundle"
  mkdir -p "$bundle/Contents/Resources"
  /usr/bin/plutil -create xml1 "$bundle/Contents/Info.plist"
  /usr/bin/plutil -insert CFBundleIdentifier -string dev.idy.sing-box-flutter.smoke-capabilities "$bundle/Contents/Info.plist"
  /usr/bin/plutil -insert CFBundlePackageType -string BNDL "$bundle/Contents/Info.plist"
  /usr/bin/ditto "$macos_directory/Resources/PrivacyInfo.xcprivacy" "$bundle/Contents/Resources/PrivacyInfo.xcprivacy"
  /usr/bin/ditto "$capabilities_path" "$bundle/Contents/Resources/core-capabilities.json"
}

if [[ "$ci_tun" == true ]]; then
  # The default smoke already ran earlier in the CI job. This explicit path only
  # compiles the real-traffic executable, then elevates that one process.
  create_capabilities_bundle
  xcrun swiftc -swift-version 5 -D SINGBOX_SELECTABLE_CORE -target "$host_architecture-apple-macos12.0" \
    -sdk "$sdk" -F "$native_framework" -F "$flutter_framework" \
    -framework Libbox -framework FlutterMacOS -framework Network -framework Security \
    -framework SystemConfiguration -framework CoreFoundation -lresolv \
    -Xlinker -rpath -Xlinker "$flutter_framework" \
    "${user_tun_shared_sources[@]}" "$macos_directory/Classes/NativeCore.swift" \
    "$macos_directory"/Classes/UserTun/*.swift "$script_directory/CITunTrafficSmoke.swift" \
    -o "$smoke_directory/CITunTrafficSmoke"
  # Do not preserve the runner's environment, credentials or proxy variables.
  /usr/bin/sudo -n /usr/bin/env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin \
    GITHUB_ACTIONS=true RUNNER_OS=macOS SINGBOX_CI_TUN_SMOKE=1 \
    "$smoke_directory/CITunTrafficSmoke" --ci-tun
  exit 0
fi

if [[ -f "$script_directory/SharedStorageSmoke.swift" ]]; then
  xcrun swiftc -swift-version 5 -target "$host_architecture-apple-macos12.0" -sdk "$sdk" \
    "$macos_directory/Classes/Shared/SharedTypes.swift" "$macos_directory/Classes/Shared/SharedState.swift" \
    "$script_directory/SharedStorageSmoke.swift" -o "$smoke_directory/SharedStorageSmoke"
  "$smoke_directory/SharedStorageSmoke"
fi
# The host sources are compiled and linked below; only the other architecture
# needs separate typechecks.
for architecture in arm64 x86_64; do
  if [[ "$architecture" == "$host_architecture" ]]; then continue; fi
  printf 'Typecheck shared NE and both Flutter controllers for macOS 12 (%s).\n' "$architecture"
  xcrun swiftc -typecheck -swift-version 5 -application-extension -D SINGBOX_SELECTABLE_CORE \
    -target "$architecture-apple-macos12.0" -sdk "$sdk" -F "$native_framework" \
    "$macos_directory"/Classes/Shared/*.swift "$macos_directory"/Classes/PacketTunnel/*.swift
  xcrun swiftc -typecheck -swift-version 5 -target "$architecture-apple-macos12.0" -sdk "$sdk" -F "$flutter_framework" \
    "$macos_directory"/Classes/Shared/*.swift "$macos_directory/Classes/NE/SingBoxFlutterPlugin.swift"
  xcrun swiftc -typecheck -swift-version 5 -D SINGBOX_SELECTABLE_CORE -target "$architecture-apple-macos12.0" \
    -sdk "$sdk" -F "$native_framework" -F "$flutter_framework" \
    "${user_tun_shared_sources[@]}" "$macos_directory/Classes/NativeCore.swift" "$macos_directory"/Classes/UserTun/*.swift
done

# The extension links without Flutter; the NE controller links without Go.
xcrun swiftc -emit-library -module-name sing_box_flutter -swift-version 5 -application-extension -D SINGBOX_SELECTABLE_CORE \
  -target "$host_architecture-apple-macos12.0" -sdk "$sdk" -F "$native_framework" \
  -framework Libbox -framework NetworkExtension -framework Network -framework Security \
  -framework SystemConfiguration -framework CoreFoundation -lresolv \
  "$macos_directory"/Classes/Shared/*.swift "$macos_directory"/Classes/PacketTunnel/*.swift \
  -o "$smoke_directory/PacketTunnel.dylib"
xcrun otool -L "$smoke_directory/PacketTunnel.dylib" > "$smoke_directory/extension-dependencies"
if grep -Eq 'FlutterMacOS|Flutter.framework|Flutter/Flutter' "$smoke_directory/extension-dependencies"; then
  printf 'The Packet Tunnel extension unexpectedly links Flutter.\\n' >&2
  exit 1
fi

# Go-free NE controller: link it independently and reject accidental Go symbols.
xcrun swiftc -emit-library -module-name sing_box_flutter -swift-version 5 -target "$host_architecture-apple-macos12.0" \
  -sdk "$sdk" -F "$flutter_framework" -framework FlutterMacOS -framework NetworkExtension \
  "$macos_directory"/Classes/Shared/*.swift "$macos_directory/Classes/NE/SingBoxFlutterPlugin.swift" \
  -o "$smoke_directory/NEController.dylib"
xcrun nm "$smoke_directory/NEController.dylib" > "$smoke_directory/ne-symbols"
xcrun otool -L "$smoke_directory/NEController.dylib" >> "$smoke_directory/ne-symbols"
if grep -Eq 'Libbox|_crosscall2|_runtime\.rt0_go|_runtime\.main|_go_seq_' "$smoke_directory/ne-symbols"; then
  printf 'The NE Flutter controller unexpectedly links Go/libbox.\n' >&2
  exit 1
fi

create_capabilities_bundle
xcrun swiftc -swift-version 5 -D SINGBOX_SELECTABLE_CORE -target "$host_architecture-apple-macos12.0" \
  -sdk "$sdk" -F "$native_framework" -F "$flutter_framework" \
  -framework Libbox -framework FlutterMacOS -framework Network -framework Security \
  -framework SystemConfiguration -framework CoreFoundation -lresolv \
  -Xlinker -rpath -Xlinker "$flutter_framework" \
  "${user_tun_shared_sources[@]}" "$macos_directory/Classes/NativeCore.swift" \
  "$macos_directory"/Classes/UserTun/*.swift "$script_directory/UserTunRuntimeSmoke.swift" \
  "$script_directory/NativeCoreSmoke.swift" \
  -o "$smoke_directory/NativeCoreSmoke"
unset TS_PERMIT_TOOLCHAIN_MISMATCH
"$smoke_directory/NativeCoreSmoke"
printf 'macOS native smoke passed. Signed NE consent and privileged utun traffic require host testing.\n'
