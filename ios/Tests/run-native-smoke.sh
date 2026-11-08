#!/bin/bash
# SPDX-License-Identifier: GPL-3.0-or-later
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  printf 'Usage: %s <booted-simulator-uuid> [packet-tunnel-executable]\n' "$0" >&2
  exit 64
fi

script_directory="$(dirname "$(realpath "$0")")"
ios_directory="$(dirname "$script_directory")"
package_directory="$(dirname "$ios_directory")"
framework_directory="$ios_directory/Frameworks/Libbox.xcframework"
expected_version="$(/usr/bin/plutil -extract sing_box_version raw -o - "$package_directory/native-core.json")"
device_sdk="$(xcrun --sdk iphoneos --show-sdk-path)"
simulator_sdk="$(xcrun --sdk iphonesimulator --show-sdk-path)"
smoke_directory="$(mktemp -d "${TMPDIR:-/tmp}/sing-box-ios-smoke.XXXXXX")"
trap 'rm -rf "$smoke_directory"' EXIT

for slice in ios-arm64 ios-arm64_x86_64-simulator; do
  if [[ ! -d "$framework_directory/$slice/Libbox.framework" ]]; then
    printf 'Missing %s. Install the pinned Apple native artifact first.\n' "$framework_directory/$slice/Libbox.framework" >&2
    exit 1
  fi
done

# Keep this list aligned with the Core subspec. Do not use a mocked libbox module.
native_link_flags=(
  -framework Libbox -framework NetworkExtension -framework Network
  -framework Security -framework SystemConfiguration -framework CoreFoundation
  -framework UIKit -lresolv
)

printf 'Compile and link the extension for physical arm64, iOS 15.\n'
xcrun --sdk iphoneos swiftc -emit-library -module-name sing_box_flutter \
  -swift-version 5 -application-extension -target arm64-apple-ios15.0 \
  -sdk "$device_sdk" -F "$framework_directory/ios-arm64" \
  "${native_link_flags[@]}" \
  "$ios_directory/Classes/Shared/SharedState.swift" \
  "$ios_directory/Classes/PacketTunnel/ApplePlatformInterface.swift" \
  "$ios_directory/Classes/PacketTunnel/SingBoxPacketTunnelProvider.swift" \
  -o "$smoke_directory/PacketTunnel.dylib"

simulator_architecture="$(uname -m)"
case "$simulator_architecture" in
  arm64|x86_64) ;;
  *) printf 'Unsupported simulator host architecture.\n' >&2; exit 1 ;;
esac

printf 'Build and run native runtime contract checks in the supplied simulator.\n'
xcrun --sdk iphonesimulator swiftc -swift-version 5 \
  -target "$simulator_architecture-apple-ios15.0-simulator" \
  -sdk "$simulator_sdk" -F "$framework_directory/ios-arm64_x86_64-simulator" \
  "${native_link_flags[@]}" \
  "$ios_directory/Classes/Shared/SharedState.swift" \
  "$script_directory/NativeCoreSmoke.swift" \
  -o "$smoke_directory/NativeCoreSmoke"

# Do not set TS_PERMIT_TOOLCHAIN_MISMATCH: the runtime assertion must catch
# mismatches that a successful native link and framework version string cannot.
unset TS_PERMIT_TOOLCHAIN_MISMATCH SIMCTL_CHILD_TS_PERMIT_TOOLCHAIN_MISMATCH
xcrun simctl spawn "$1" "$smoke_directory/NativeCoreSmoke" "$expected_version"

if [[ $# -eq 2 ]]; then
  printf 'Check that the embedded extension does not link Flutter.\n'
  linked_dependencies="$(xcrun otool -L "$2")"
  if [[ "$linked_dependencies" == *Flutter.framework* || "$linked_dependencies" == *Flutter/Flutter* ]]; then
    printf 'The Packet Tunnel extension unexpectedly links Flutter.\n' >&2
    exit 1
  fi
fi

printf 'Native smoke checks passed. Device VPN behavior still requires signed-device testing.\n'
