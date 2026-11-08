#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
set -euo pipefail

script_directory="$(dirname "$(realpath "$0")")"
ios_directory="$(dirname "$script_directory")"
package_directory="$(dirname "$ios_directory")"
test_directory="$(mktemp -d "${TMPDIR:-/tmp}/sing-box-apple-tests.XXXXXX")"
trap 'rm -rf "$test_directory"' EXIT

printf 'Run Keychain ownership, error, migration and preferences contracts on macOS.\n'
xcrun --sdk macosx swiftc -swift-version 5 \
  -target "$(uname -m)-apple-macos12.0" -sdk "$(xcrun --sdk macosx --show-sdk-path)" \
  "$ios_directory/Classes/Shared/TunnelFailure.swift" \
  "$ios_directory/Classes/Shared/KeychainConfigurationStore.swift" \
  "$ios_directory/Classes/Shared/SharedState.swift" \
  "$ios_directory/Classes/Shared/TunnelPreferences.swift" \
  "$script_directory/KeychainStoreTests.swift" \
  -o "$test_directory/KeychainStoreTests"
"$test_directory/KeychainStoreTests"

# Use installed real libbox/Flutter headers, never a fake binding module. This also
# accepts the original checkout's artifact without copying it into the worktree.
framework_directory="${LIBBOX_XCFRAMEWORK:-$ios_directory/Frameworks/Libbox.xcframework}"
flutter_directory="${FLUTTER_ROOT:-$(dirname "$(dirname "$(realpath "$(command -v flutter)")")")}"
ios_framework_directory="$framework_directory/ios-arm64"
ios_flutter_directory="$flutter_directory/bin/cache/artifacts/engine/ios/Flutter.xcframework/ios-arm64"
macos_flutter_directory="$flutter_directory/bin/cache/artifacts/engine/darwin-x64/FlutterMacOS.xcframework/macos-arm64_x86_64"
if [[ ! -d "$ios_framework_directory/Libbox.framework" || ! -d "$ios_flutter_directory/Flutter.framework" ]]; then
  printf 'Install the Apple libbox artifact and Flutter iOS engine before source checks.\n' >&2
  exit 1
fi

printf 'Compile and link the real iOS 15 Packet Tunnel without Flutter.\n'
xcrun --sdk iphoneos swiftc -emit-library -module-name sing_box_flutter \
  -swift-version 5 -application-extension -target arm64-apple-ios15.0 \
  -sdk "$(xcrun --sdk iphoneos --show-sdk-path)" -F "$ios_framework_directory" \
  -framework Libbox -framework NetworkExtension -framework Network \
  -framework Security -framework LocalAuthentication -framework SystemConfiguration \
  -framework CoreFoundation -framework UIKit -lresolv \
  "$ios_directory"/Classes/Shared/*.swift \
  "$ios_directory"/Classes/PacketTunnel/*.swift \
  -o "$test_directory/PacketTunnel.dylib"
linked_dependencies="$(xcrun otool -L "$test_directory/PacketTunnel.dylib")"
if [[ "$linked_dependencies" == *Flutter* ]]; then
  printf 'The Packet Tunnel unexpectedly links Flutter.\n' >&2
  exit 1
fi

printf 'Typecheck the iOS 15 Flutter plugin with its actual engine.\n'
xcrun --sdk iphoneos swiftc -typecheck -swift-version 5 -target arm64-apple-ios15.0 \
  -sdk "$(xcrun --sdk iphoneos --show-sdk-path)" \
  -F "$ios_framework_directory" -F "$ios_flutter_directory" \
  "$ios_directory"/Classes/Shared/*.swift \
  "$ios_directory/sing_box_flutter/Sources/sing_box_flutter/SingBoxFlutterPlugin.swift"

macos_framework_directory=""
for candidate in "$framework_directory"/macos-*; do
  if [[ -d "$candidate/Libbox.framework" ]]; then macos_framework_directory="$candidate"; break; fi
done
if [[ -z "$macos_framework_directory" ]]; then
  # gomobile's Objective-C bindings are platform-independent. Typechecking their
  # actual headers with the macOS SDK catches Apple availability and Flutter API
  # differences, but cannot establish that a macOS Go binary links or executes.
  macos_framework_directory="$ios_framework_directory"
  printf 'macOS slice absent: check real binding headers only; macOS link/runtime remains unverified.\n'
else
  printf 'Compile and link the macOS 12 Packet Tunnel against its real libbox slice.\n'
  xcrun --sdk macosx swiftc -emit-library -module-name sing_box_flutter \
    -swift-version 5 -application-extension -Xlinker -fatal_warnings -target "$(uname -m)-apple-macos12.0" \
    -sdk "$(xcrun --sdk macosx --show-sdk-path)" -F "$macos_framework_directory" \
    -framework Libbox -framework NetworkExtension -framework Network \
    -framework Security -framework LocalAuthentication -framework SystemConfiguration \
    -framework CoreFoundation -framework AppKit -lresolv \
    "$ios_directory"/Classes/Shared/*.swift \
    "$ios_directory"/Classes/PacketTunnel/*.swift \
    -o "$test_directory/MacPacketTunnel.dylib"
  printf 'Execute real libbox/WGC native contract checks on macOS 12+.\n'
  xcrun --sdk macosx swiftc -swift-version 5 -Xlinker -fatal_warnings \
    -target "$(uname -m)-apple-macos12.0" -sdk "$(xcrun --sdk macosx --show-sdk-path)" \
    -F "$macos_framework_directory" \
    -framework Libbox -framework NetworkExtension -framework Network \
    -framework Security -framework LocalAuthentication -framework SystemConfiguration \
    -framework CoreFoundation -framework AppKit -lresolv \
    "$ios_directory"/Classes/Shared/*.swift "$script_directory/NativeCoreSmoke.swift" \
    -o "$test_directory/MacNativeCoreSmoke"
  expected_version="$(/usr/bin/plutil -extract sing_box_version raw -o - "$package_directory/native-core.json")"
  (
    unset TS_PERMIT_TOOLCHAIN_MISMATCH
    "$test_directory/MacNativeCoreSmoke" "$expected_version" "$@"
  )
fi
for architecture in arm64 x86_64; do
  printf 'Typecheck canonical macOS 12 provider and FlutterMacOS plugin (%s).\n' "$architecture"
  xcrun --sdk macosx swiftc -typecheck -swift-version 5 -application-extension \
    -target "$architecture-apple-macos12.0" -sdk "$(xcrun --sdk macosx --show-sdk-path)" \
    -F "$macos_framework_directory" "$ios_directory"/Classes/Shared/*.swift \
    "$ios_directory"/Classes/PacketTunnel/*.swift
  xcrun --sdk macosx swiftc -typecheck -swift-version 5 \
    -target "$architecture-apple-macos12.0" -sdk "$(xcrun --sdk macosx --show-sdk-path)" \
    -F "$macos_framework_directory" -F "$macos_flutter_directory" \
    "$ios_directory"/Classes/Shared/*.swift \
    "$ios_directory/sing_box_flutter/Sources/sing_box_flutter/SingBoxFlutterPlugin.swift"
done

printf 'Check both CocoaPods specs and canonical source links.\n'
for platform in ios macos; do
  LANG=en_US.UTF-8 pod ipc spec "$package_directory/$platform/sing_box_flutter.podspec" >"$test_directory/$platform.podspec.json"
  python3 -I - "$package_directory" "$platform" "$test_directory/$platform.podspec.json" <<'PY'
import glob
import json
from pathlib import Path
import sys

root, platform, spec_file = Path(sys.argv[1]), sys.argv[2], Path(sys.argv[3])
spec = json.loads(spec_file.read_text())
for subspec in spec["subspecs"]:
    pattern = subspec["source_files"]
    actual = {Path(p).resolve() for p in glob.glob(str(root / platform / pattern), recursive=True)}
    canonical = {Path(p).resolve() for p in glob.glob(str(root / "ios" / pattern), recursive=True)}
    assert actual and actual == canonical, f"Missing canonical {subspec['name']} sources"
    if subspec["name"] == "PacketTunnel":
        assert set(subspec["dependencies"]) == {"sing_box_flutter/Core"}, "Extension must not depend on Flutter"
    if subspec["name"] == "Core":
        assert subspec["vendored_frameworks"] == "Frameworks/Libbox.xcframework"
PY
done
printf 'Apple source checks passed. Signed host/extension VPN tests still require matching entitlements.\n'
