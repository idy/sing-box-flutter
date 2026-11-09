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
