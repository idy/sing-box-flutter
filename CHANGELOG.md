## Unreleased

- Add Linux x86_64/arm64 support with a bundled, source-built Go helper, native
  TUN/routing, configuration validation and connection events.
- Add Linux capabilities checks, automatic cleanup on application exit, source
  receipt validation in CMake, a GTK example and Linux CI/runtime smoke checks.

## 0.3.0

- Select compiled protocols, endpoints, DNS transports, TLS features, APIs and
  services through the host application's `sing_box_flutter.core` pubspec block.
- Build a thin mobile binding with only selected constructors using the pinned
  Tailscale Go toolchain; retain the existing Flutter VPN control API.
- Keep selected AAR/XCFramework outputs in the host project with source fingerprints,
  capability manifests and integrity checks in Gradle and CocoaPods.
- Link the Go engine only into the iOS Packet Tunnel extension. The main app uses
  structural/capability preflight; full native validation occurs before connecting.
- Add WGC minimal, WGC + WireGuard + Tailscale, and full mobile example profiles.
- Publish source and build tooling without universal precompiled libbox release
  assets. Build the native core from the host app before compiling Flutter.
- Preserve explicit legacy standard-core builds and historical artifact installs.

## 0.2.0

- Register a WGC endpoint in the existing libbox runtime, directly using
  `github.com/idy/wgc` and the sing-box userspace stack.
- Add native transport factory hooks and pin their adapter/build inputs.
- Preserve scoped private Go module access settings during native builds.
- Keep the existing Flutter API and Android/iOS platform implementation.

## 0.1.0

- Add Android and iOS native VPN control through typed Dart APIs.
- Add connection-state events, native configuration validation, explicit VPN
  consent, and service lifecycle management independent of the Flutter UI.
- Add Android foreground VpnService and an iOS Packet Tunnel provider for host
  extension targets.
- Pin sing-box 1.14.4 and the Tailscale Go toolchain for native core builds,
  including Tailscale endpoint support.
- Add a direct-routing example and host integration documentation.

Native core artifacts must be installed separately. iOS hosts must configure and
sign their own Packet Tunnel extension and matching App Group entitlements.
