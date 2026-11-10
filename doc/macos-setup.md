# macOS host setup

macOS 12+ supports two build-time backends. Both expose the existing Dart API.
Choose one in the **host application's** `pubspec.yaml`:

```yaml
sing_box_flutter:
  macos:
    mode: ne # ne or user_tun; omitted mode defaults to ne
  core:
    inbounds: [tun]
    outbounds: [direct]
    endpoints: [wgc]
    dns: [udp, tcp, fakeip]
```

The `core` categories have the same meaning as Android/iOS. A `macos` block
without `core` uses the default TUN/direct/local-TCP-UDP DNS selection. This is
not a runtime selector: changing the mode requires rebuilding the native core
and Flutter app. Unknown modes or keys fail explicitly.

Install Python 3.11+, Git, Xcode with its macOS SDK, and CocoaPods. Resolve packages
and compile the universal arm64/x86_64 core from the host directory:

```bash
flutter pub get
```

```bash
dart run sing_box_flutter build --target macos
```

The selected framework, capabilities and integrity receipt live in the host's
`.dart_tool/sing_box_flutter/`. CocoaPods verifies the current sources, pubspec,
backend fingerprint and artifact hashes. Rebuild and rerun `pod install` after
changing the mode or core selection. Flutter's Swift Package Manager integration
must be disabled for this CocoaPods plugin, as in the checked-in example.

## Network Extension (`ne`)

The default mode runs the core inside a separately signed `NEPacketTunnelProvider`.
The Flutter app manages VPN preferences and status without linking the Go runtime.
The tunnel survives Flutter engine teardown and application UI restarts.

1. Set `platform :osx, '12.0'` in the host's `macos/Podfile`, and set the same
   deployment target on the Runner and extension targets.
2. Create a macOS Packet Tunnel **app extension** named `PacketTunnel`, embed it
   in the app's PlugIns directory, and use a unique bundle identifier.
3. Give the app and extension matching App Group entitlements. Enable Network
   Extensions with the `packet-tunnel-provider` value on both signed targets.
   The extension must also have the required sandbox network client/server rights.
4. Add only the plugin's `PacketTunnel` subspec to the extension target:

```ruby
target 'PacketTunnel' do
  pod 'sing_box_flutter/PacketTunnel', :path => 'Flutter/ephemeral/.symlinks/plugins/sing_box_flutter/macos'
end
```

Keep Flutter's generated plugin installation inside Runner. Do not install
`FlutterMacOS` in the extension target. Keep Runner's Flutter/Pods xcconfig on the
Runner target rather than at project level: the extension must inherit only its
own `Pods-PacketTunnel` configuration. Otherwise Swift can resolve the Go-free
Runner module instead of the module that exports the tunnel provider. The
example configuration helper separates these settings. The provider source is:

```swift
import sing_box_flutter

final class PacketTunnelProvider: SingBoxPacketTunnelProvider {}
```

Set `NSExtensionPointIdentifier` to `com.apple.networkextension.packet-tunnel` and
`NSExtensionPrincipalClass` to `$(PRODUCT_MODULE_NAME).PacketTunnelProvider` in the
extension's Info.plist. Configure an Apple team and provisioning profiles that
permit Network Extension and App Groups on both targets.

Initialize with the exact entitlement and extension identifiers. macOS also
supports Team-ID-prefixed App Groups such as `ABCDE12345.com.example.shared`;
use that exact value in both entitlements and `appGroup`. A non-nil container URL
alone does not establish access, so initialization verifies the shared storage
operations as well:

```dart
await vpn.initialize(
  appGroup: 'group.com.example.app',
  providerBundleIdentifier: 'com.example.app.PacketTunnel',
);
```

`checkConfig` runs structural/capability preflight in the app; full engine
validation occurs at extension startup. `requestPermission` installs the VPN
profile through macOS and can be cancelled. Observe the status stream after
`start`; acceptance of the request does not guarantee successful connection.

## Direct user TUN (`user_tun`)

This mode embeds the core in the host process. The native macOS sing-box backend
creates `utun` and manages routing, DNS and default interface monitoring directly.
It does not use a Network Extension, PacketTunnel target, App Group or VPN profile.
It retains the feature-selected registry and the same start/stop/status channels.

Set `macos.mode: user_tun`, remove any PacketTunnel dependency and embedding from
the host project, and disable App Sandbox in all host build configurations.
Build the core again, rerun `pod install`, then build the app. Initialize without
Apple identifiers:

```dart
await vpn.initialize();
```

`checkConfig` runs only while the standalone runtime is idle. Configuration
checks reserve the native runtime through constructor cleanup: another check or
start returns `busy` until validation finishes, and checks during connection
startup, an active tunnel or teardown also return `busy`. Status queries and
explicit stop requests remain responsive.

When automatic routing and DNS are enabled, the backend registers the effective
TUN DNS addresses as a temporary system resolver. Stopping or failing startup
removes that registration without overwriting physical-network DNS settings.
`dns_mode: disabled` and `auto_route: false` leave system DNS unchanged. macOS can
still choose more-specific domain/scoped resolvers; this is default-resolver
integration, not a policy that overrides every application's DNS mechanism.

Destination-based `route_address` and `route_exclude_address` are supported.
Darwin does not implement UID/UID-range filters, Android-user or package filters,
or `strict_route` for this backend; those options are rejected rather than
silently ignored. Default uplink monitoring supports both IPv4 and IPv6 paths
and excludes the core's own tunnel interfaces.

Creating a native `utun` and updating system routes/DNS requires **an already
privileged process (effective UID 0)**. `requestPermission` checks that privilege
and returns false otherwise. `start` returns a fixed `permission_required` error
when it is absent. This backend does not display an administrator authentication
dialog, elevate the app or install a privileged helper. A product that needs an
ordinary-user Flutter UI must supply its own separately authorized privileged
service integration, or choose NE mode. Do not run development build tools with
administrator privileges to work around a failed permission check.

The core survives Flutter engine detachment within the same process. Quitting the
host process ends this mode; call `stop()` first so sing-box can cleanly restore
routes and DNS. NE mode has OS-owned lifetime instead.

## Checked-in example

The example's `macos/configure_project.rb` reads its pubspec mode and configures
extension embedding and sandbox entitlements accordingly. CocoaPods invokes it
automatically during `pod install`. To run it separately, use the Ruby environment
that provides CocoaPods' `xcodeproj` gem (Homebrew's CocoaPods may use a different
Ruby from `/usr/bin/ruby`). From `example/`, after selecting the mode:

```bash
ruby macos/configure_project.rb
```

```bash
dart run sing_box_flutter build --target macos
```

```bash
flutter build macos --debug
```

For NE, replace the example team, bundle IDs and App Group with your own before
launching. Pass `APP_GROUP` and `TUNNEL_BUNDLE_ID` using `--dart-define` as with iOS.
The user TUN mode does not need these defines. The profile switcher preserves the
macOS mode while changing protocol features.

## Verification

The Native core CI workflow runs a `ne` / `user_tun` matrix on hosted macOS
runners. It builds the universal core and unsigned example, verifies actual
app/extension resource lookup, and checks native lifecycle, cancellation, DNS
ownership and IPv6-aware path handling. Dart/Python tests also cover strict
pubspec parsing, immutable requests, fingerprints, host-local artifacts and
rebuild guards. The default native smoke never changes host networking.

The `user_tun` job additionally runs a CI-only traffic test with administrator
privileges on its disposable runner. It creates a real `utun`, routes only one
reserved benchmarking destination to a local HTTP fixture through sing-box, and
checks interface cleanup after stop. DNS is disabled for that test; neither the
default route nor physical-network DNS/proxy settings is changed. The script and
executable require explicit CI opt-in and refuse a normal local invocation.
This tests real TUN packet flow rather than compilation alone.

Unsigned CI cannot install and authorize a Network Extension. Signed NE
activation and traffic require an appropriately provisioned Apple signing
identity and permission-capable test environment; a successful build does not
establish those permissions. The real TUN test also does not replace live DNS,
external endpoint or network-transition testing.

Before distribution, validate a signed NE app and an explicitly authorized
privileged user TUN host on macOS: IPv4/IPv6 traffic, DNS, routing exclusions,
network transitions, sleep/wake, permission cancellation, restart and cleanup.
