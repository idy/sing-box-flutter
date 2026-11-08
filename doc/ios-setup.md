# iOS setup

The iOS backend requires iOS 15 or later and CocoaPods. A signed host app and a
separately signed Packet Tunnel Network Extension are required for a real VPN.
The plugin does not create an extension target or provision Apple capabilities.

The Flutter process manages VPN preferences with `NETunnelProviderManager`, checks
configuration with libbox, and passes configuration through an App Group container.
The actual sing-box service, TUN descriptor, and network monitor run exclusively in
the extension process. Canceling a Flutter event subscription, detaching its engine,
or terminating the host app does not stop the VPN.

## Install the pinned native artifact

Install the native build artifact before `pod install`:

```text
ios/Frameworks/Libbox.xcframework
```

Use the repository's native artifact tooling and the exact pins in
`native-core.json`: sing-box **v1.14.4**, its pinned Tailscale Go toolchain, and
SagerNet gomobile **v0.1.12**. The framework needs device arm64 and simulator
arm64/x86_64 slices. Do not substitute another libbox release: its generated Swift
method signatures and platform protocol can change. Build artifacts are installed
separately from the source package.

This package supports CocoaPods only. Its generated SwiftPM template referenced a
missing `FlutterFramework` package and has been removed. Disable Swift Package
Manager for the host project, for example in the host `pubspec.yaml` on Flutter
versions supporting project configuration:

```yaml
flutter:
  config:
    enable-swift-package-manager: false
```

## Add and sign the extension target

1. In the host's Xcode project, add a **Network Extension / Packet Tunnel** target,
   for example `PacketTunnel`. Set its deployment target to iOS **15.0** or later.
2. Give the extension its own bundle identifier, such as
   `dev.example.app.PacketTunnel`, beneath the host app's identifier. Use the same
   development team and correct provisioning profiles for both targets.
3. Enable **App Groups** on both targets and select the same registered group,
   for example `group.dev.example.app`. Enable **Network Extensions / Packet
   Tunnel** on the extension. The host's signing profile must permit managing the
   VPN configuration; enable the same Network Extension capability on the host.
4. Embed the extension in the host app's **Embed App Extensions** build phase and
   ensure it is a target dependency. Set `APPLICATION_EXTENSION_API_ONLY = YES`
   and `SKIP_INSTALL = YES` on the extension.

Both targets' entitlements include:

```xml
<key>com.apple.security.application-groups</key>
<array>
  <string>group.dev.example.app</string>
</array>
<key>com.apple.developer.networking.networkextension</key>
<array>
  <string>packet-tunnel-provider</string>
</array>
```

Select those entitlements files in each target's **Code Signing Entitlements**
setting. Entitlements in a file alone do not replace capabilities in the Apple
Developer portal and provisioning profile.

The extension's `Info.plist` includes:

```xml
<key>NSExtension</key>
<dict>
  <key>NSExtensionPointIdentifier</key>
  <string>com.apple.networkextension.packet-tunnel</string>
  <key>NSExtensionPrincipalClass</key>
  <string>$(PRODUCT_MODULE_NAME).PacketTunnelProvider</string>
</dict>
```

Add this Swift file to **only** the extension target:

```swift
import sing_box_flutter

final class PacketTunnelProvider: SingBoxPacketTunnelProvider {}
```

The App Group is read from
`NETunnelProviderProtocol.providerConfiguration["appGroup"]`, which the plugin
writes during `requestPermission`. No additional custom `Info.plist` key is needed.

## Configure CocoaPods

The pod's default subspec is `Plugin`, which Flutter installs in the host target.
The extension must explicitly depend on `PacketTunnel` and must not inherit all
Runner pods. Keep Flutter's existing Podfile setup and installation hooks:

```ruby
platform :ios, '15.0'

# Existing Flutter podhelper setup goes here.

target 'Runner' do
  use_frameworks! :linkage => :static
  flutter_install_all_ios_pods File.dirname(File.realpath(__FILE__))
end

target 'PacketTunnel' do
  use_frameworks! :linkage => :static
  pod 'sing_box_flutter/PacketTunnel',
      :path => '.symlinks/plugins/sing_box_flutter/ios'
end

post_install do |installer|
  installer.pods_project.targets.each do |target|
    flutter_additional_ios_build_settings(target)
  end
end
```

If nesting the extension under Runner, use `inherit! :search_paths`, not
`inherit! :complete`. Do not call `flutter_install_all_ios_pods` inside the
extension target. The extension's resolved sing_box_flutter pod variant contains
`Core` and `PacketTunnel`, with no Flutter dependency; the host variant contains
`Core` and `Plugin`. The Swift module name for both is `sing_box_flutter`.

Run `flutter pub get`, then `pod install` in the host's `ios` directory. Open the
resulting `.xcworkspace`. Do not manually embed the static `Libbox.framework`;
CocoaPods links the XCFramework selected for the target platform.

## Initialize and connect

Pass the exact signed group and extension bundle identifier to the Dart API:

```dart
final vpn = SingBoxFlutter();
await vpn.initialize(
  appGroup: 'group.dev.example.app',
  providerBundleIdentifier: 'dev.example.app.PacketTunnel',
  notificationTitle: 'Example VPN',
);
final allowed = await vpn.requestPermission();
if (allowed) {
  await vpn.checkConfig(config);
  await vpn.start(config: config, name: 'Example');
}
```

Use the status stream to observe the result. `start` returns when the system
accepts the request. `connected` is reported only after the extension has applied
network settings and libbox has successfully started a TUN inbound. A proxy-only
configuration fails startup rather than reporting a packet tunnel connection.
`getVersion` is available before initialization. All other methods require
`initialize` to complete. VPN consent cancellation returns `false`; signing,
configuration, and preference failures return a platform error.

Each start writes an atomic, versioned App Group configuration with a new session
identifier. The provider can load it when started from iOS Settings without a
running Flutter engine. Calling `start` while the tunnel is active or transitioning
returns `busy`; wait for `disconnected` after `stop` before changing configuration.
The plugin serializes method requests, and the extension serializes libbox
start/stop/sleep/wake operations. It observes OS connection changes, including VPN
revocation. A new event listener receives the current system status.

## Networking and supported features

The platform adapter applies TUN IPv4/IPv6 addresses, MTU, libbox-computed route
ranges and exclusions, DNS settings, and configured HTTP/HTTPS proxy settings with
`NEPacketTunnelNetworkSettings`. It obtains the utun descriptor with upstream
`LibboxGetTunnelFileDescriptor`. libbox duplicates the descriptor; the adapter
never closes NEPacketTunnelFlow's original descriptor. A lifecycle-owned
`NWPathMonitor` reports default interface, expensive/constrained path changes, and
interface addresses/flags/MTU to libbox. Monitor startup and network settings waits
are bounded and report failure.

The initial backend deliberately returns `unsupported` for per-app TUN routing,
`strict_route`, process ownership lookup, neighbor discovery, local shell/SSH/SFTP
server access, bridge creation, notification delivery, SSH agent forwarding,
manual service reload, runtime proxy toggles, and native crash triggers. Wi-Fi
SSID/BSSID is unavailable (`nil`); SSID-dependent routing must not be used. There
is no platform local-DNS transport override. No raw libbox logs, notification auth
URLs, or configuration dumps are forwarded to Flutter. Interactive Tailscale login
notifications are not implemented; supply the endpoint's supported authentication
configuration securely or reuse its persisted state. Do not enable optional
features absent from the installed native build profile.

Provider messages use a small versioned JSON envelope:
`{"version":1,"command":"getStatus"}` or `getVersion`. The provider returns a
status snapshot or version. Invalid or unknown commands return an explicit error.
The public Flutter status remains governed by the system connection and the
provider's startup result, with a bounded timeout for message replies.

## Shared data and diagnostics

The group contains `SingBoxFlutter/configuration.json`, `status.json`, isolated
`Validation` directories for host-side config checks, and `Runtime` directories for
the extension. The directory is excluded from backups and uses
`completeUntilFirstUserAuthentication` data protection with restricted POSIX
permissions. This supports reconnecting while the device is locked after the first
unlock. Configurations, Tailscale state, libbox crash reports, and core snapshots
can contain credentials. Treat the App Group as sensitive storage and do not
upload its contents as unredacted diagnostics. The last configuration is retained
after stop so the system can start the saved VPN again.

Flutter errors and events use fixed, bounded messages rather than upstream error
text, because parse errors can echo JSON or authentication keys. Check signing,
the group identifiers, and system extension logs locally when troubleshooting.
After an extension error, `getStatus` can return its persisted safe error for the
matching session. A normal `stop` clears it once the VPN is stopped.

## Validation and example host changes

A real device with correctly provisioned capabilities is required to verify VPN
consent, packet forwarding, DNS, network changes, locked-device reconnects,
revocation, and survival after terminating Flutter. Simulator compilation verifies
bindings and target integration, not a functional device VPN. Do not claim a
connection based only on compilation or configuration validation.

A generated Flutter example needs the same changes as any host: iOS 15 deployment,
a CocoaPods Podfile, SwiftPM disabled, a signed and embedded `PacketTunnel` target,
matching App Group/Network Extension entitlements, its principal-class plist entry,
the small provider subclass, and matching identifiers passed to `initialize`.
These host target files are intentionally outside the plugin implementation.

The implementation's upstream source attribution is in `ios/NOTICE`; preserve it
and the GPL license and corresponding source obligations when distributing the
extension and libbox binaries.
