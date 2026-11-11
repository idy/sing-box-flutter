# Apple Swift Package Manager integration

Version 0.3.1 supports Swift Package Manager (SPM) on iOS 15+ and macOS 12+,
while retaining CocoaPods. Use Flutter 3.47.2+, Xcode and Python 3.11+.
SPM integration uses a host-selected source core; historical standard-core
installations can continue using CocoaPods with SPM disabled.

## Build the host's selection first

Add the dependency and feature selection to your Flutter application's pubspec,
then enable SPM for that application:

```yaml
dependencies:
  sing_box_flutter: ^0.3.1

sing_box_flutter:
  core:
    inbounds: [tun]
    outbounds: [direct]
    endpoints: [wgc]
    dns: [udp, tcp, fakeip]
  macos:
    mode: ne # or user_tun

flutter:
  config:
    enable-swift-package-manager: true
```

Resolve packages and build each platform you need from the host directory:

```bash
flutter pub get
```

```bash
dart run sing_box_flutter build --target apple
```

```bash
dart run sing_box_flutter build --target macos
```

The builder stages the host's verified framework, capability metadata, canonical
Swift sources and build guard into:

- `ios/Flutter/ephemeral/Packages/SingBoxFlutterHost`
- `macos/Flutter/ephemeral/Packages/SingBoxFlutterHost`

These are generated, application-owned packages. Do not edit or commit them.
The dependency's checked-in SPM facade resolves this package through Flutter's
host-local package layout; no global configuration or shared pub-cache mutation
is needed. Each app retains its own feature selection and backend. A plain
Flutter build never downloads or compiles a new Go core.

Build the native core again after changing the host pubspec, core selection,
plugin version or generated package inputs. If Flutter's ephemeral directory is
removed, build the core again before building the app. A missing host package is
not a request to use a different or larger native engine.

## Network Extension targets

The normal `sing-box-flutter` product registers the Flutter controller. In NE
mode this controller does not depend on Libbox and does not link the Go engine.
The separate host-generated product `sing-box-flutter-packet-tunnel` has module
name `SingBoxFlutterPacketTunnel`; it links Libbox and has no Flutter dependency.
Shared Swift sources are compiled independently into each module so internal
state and types do not become a new public cross-module API.

The application still owns a separately signed Packet Tunnel extension. Follow
[iOS setup](ios-setup.md) or [macOS setup](macos-setup.md) for entitlements, App
Groups, target dependency and extension embedding. SPM does not create or sign
these targets.

After building the core, add the host's generated `SingBoxFlutterHost` directory
as a **local package** in Xcode. Add **only** `sing-box-flutter-packet-tunnel` to
the PacketTunnel target; Flutter manages the app's plugin product. Set
`APPLICATION_EXTENSION_API_ONLY = YES` and `SKIP_INSTALL = YES` on the extension.
The generated extension target also compiles with application-extension API
restrictions. Do not link the controller, Flutter or FlutterMacOS into the
extension.

Use the SPM module in the extension subclass:

```swift
import SingBoxFlutterPacketTunnel

class PacketTunnelProvider: SingBoxPacketTunnelProvider {}
```

A host supporting both integration methods can select the available module:

```swift
#if canImport(SingBoxFlutterPacketTunnel)
import SingBoxFlutterPacketTunnel
#else
import sing_box_flutter
#endif

class PacketTunnelProvider: SingBoxPacketTunnelProvider {}
```

Remove the explicit `sing_box_flutter/PacketTunnel` CocoaPods dependency when
using this SPM product. Keep other CocoaPods-only plugins if the app needs them.
Do not link both copies of the PacketTunnel implementation into one extension.

## macOS user TUN

Select `sing_box_flutter.macos.mode: user_tun` and rebuild the macOS core. The
generated controller then includes the user-TUN implementation and depends on
Libbox. No PacketTunnel product is generated for this backend. The host must be
unsandboxed and already running with administrator privileges; SPM neither
installs a privileged service nor elevates the app.

Switching between NE and user TUN changes the generated package manifest and
native fingerprint. Update the host's extension embedding and entitlements as
described in [macOS setup](macos-setup.md), then rebuild the core and app.

## Build-time validation

Every generated controller and extension target uses a prebuild plugin. It
checks the exact installed selection before compilation, including when the
Swift sources did not change:

- The host pubspec, selected backend, current native source recipe, framework
  path, fingerprint, framework bytes and capability manifest must still agree.
- Staged Swift sources, the package manifest and canonical integration inputs
  must match the generated selection.
- The guard emits public capability metadata as Swift source into its permitted
  plugin output directory. It does not write app bundles or carry runtime
  configurations or credentials.

A stale selection fails the build instead of linking another artifact silently.
Only the extension or user-TUN controller imports Libbox. The NE app's version
lookup and configuration preflight remain Go-free. Privacy manifests are bundled
by the SPM resource mechanism, and CocoaPods retains its existing resource-bundle
and always-run guard behavior.

## Validation

The checked-in example defaults to CocoaPods to retain that regression path.
The SPM validation tool creates a separate host with project-local SPM enabled:

```bash
python3 tool/check_apple_spm.py --platform ios --mode ne
```

```bash
python3 tool/check_apple_spm.py --platform macos --mode ne
```

```bash
python3 tool/check_apple_spm.py --platform macos --mode user_tun
```

These require Xcode and the Ruby `xcodeproj` gem for example target setup. They
build real selected cores and Flutter hosts without signing, inspect Go symbol
ownership, and prove that an incremental build rejects changed capability bytes
even when their timestamps and Swift inputs are unchanged. They do not establish
signed-device VPN forwarding, provisioning acceptance or network transitions.
