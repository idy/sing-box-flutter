## 0.2.0

- Integrate WGC as a native sing-box endpoint in the same libbox VPN runtime,
  including compiled custom transport factories and wrappers.
- Migrate WGC configuration validation and add `checkWgcConfig` / `startWgc`.
- Add macOS 12+ through shared Apple Flutter/PacketTunnel sources and a combined
  iOS, simulator, and macOS Libbox XCFramework.
- Store Apple configuration payloads in device-only shared Keychain items;
  preferences carry persistent references and the App Group holds runtime/status.
- Pin WGC and verify WGC source/registration identity in native build metadata;
  distribute dependency notices with the native artifacts.
- Add migration instructions from `wireguard_conn_flutter`.

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
