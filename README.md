# sing_box_flutter

An independent Flutter plugin for running [sing-box](https://github.com/SagerNet/sing-box)
as a native VPN on Android and iOS. Includes a reproducible **Tailscale Go** build
of libbox with Tailscale endpoints enabled.

This project is not affiliated with or endorsed by SagerNet or Tailscale.

## Platform support

Requires **Flutter 3.47.2+**. The Android integration uses AGP 9.1 with built-in
Kotlin and JDK 17; the checked-in example and CI use the same Flutter toolchain.

| Platform | Native integration | Minimum version |
| --- | --- | --- |
| Android | Foreground `VpnService`, socket protection, network monitoring | Android 7 / API 24 |
| iOS | `NEPacketTunnelProvider` in a separately signed host extension | iOS 15 |

Flutter controls the service through method channels and receives typed connection
events. Packet processing stays in the native service. Closing the UI or cancelling
a stream subscription does not disconnect the VPN.

The first release provides core version lookup, configuration validation, explicit
VPN consent, start/stop, and connection status. It accepts complete sing-box JSON;
it does not provide a profile editor, subscription manager, or Tailscale account UI.

## Install

Add the dependency to a Flutter application:

```yaml
dependencies:
  sing_box_flutter: ^0.1.0
```

```bash
flutter pub get
```

Native binaries are distributed separately from the pub.dev package. Python 3.11+
is required to install them. Run the matching command from your Flutter app:

```bash
dart run sing_box_flutter install --target android --release v0.1.0
```

```bash
dart run sing_box_flutter install --target apple --release v0.1.0
```

The installer verifies the release checksums and pinned build metadata before
placing artifacts in the resolved plugin directory. Reinstall the artifacts after
changing the package version or clearing your pub cache. Native artifacts are not
downloaded silently during a normal Dart dependency resolution.

To build them yourself instead, use the `build` command described below.

### Android host setup

The plugin declares its VPN service and required permissions. Set the host's
`minSdk` to at least 24. VPN consent requires an attached Flutter Activity.
Android runs the service in the foreground with a persistent notification.

### iOS host setup

A Flutter plugin cannot create or sign the host application's Network Extension.
Add a Packet Tunnel extension target, embed it in the app, and configure matching
App Group entitlements on the app and extension. The extension depends on the
plugin's `PacketTunnel` CocoaPods subspec and does not embed Flutter.

Follow [the complete iOS setup guide](doc/ios-setup.md) before running the app.
A physical device is required to test VPN traffic; the simulator can validate
compilation and the UI only.

## Dart API

```dart
import 'package:sing_box_flutter/sing_box_flutter.dart';

final vpn = SingBoxFlutter();

await vpn.initialize(
  // Required on iOS; omit on Android.
  appGroup: 'group.com.example.app',
  providerBundleIdentifier: 'com.example.app.PacketTunnel',
  notificationTitle: 'Example VPN',
);

final subscription = vpn.status.listen((status) {
  print(status.state); // disconnected/connecting/connected/disconnecting/error
});

await vpn.checkConfig(configJson);
if (await vpn.requestPermission()) {
  await vpn.start(config: configJson, name: 'Example VPN');
}

// start() acknowledges the request. Observe the stream for connection success.
final current = await vpn.getStatus();
final version = await vpn.getVersion();

// Disconnect explicitly; disposing the Flutter UI does not own the native VPN.
await vpn.stop();
await subscription.cancel();
```

Invalid JSON is rejected locally. Native validation and connection failures
surface as `PlatformException`; the plugin avoids including configurations or
authentication keys in its own errors. Treat your configuration JSON as sensitive
and do not write it to application logs.

Multiple plugin objects refer to the same native VPN service. Only one connection
is managed by this plugin at a time. A new event-channel subscription receives the
current native status. Use `getStatus()` to refresh a UI after it resumes.

## Tailscale support

The pinned native build includes `with_tailscale` and the upstream mobile build's
memory-saving options. An endpoint can be included in normal sing-box JSON:

```json
{
  "endpoints": [{
    "type": "tailscale",
    "tag": "tailnet",
    "hostname": "my-flutter-app",
    "system_interface": false,
    "accept_routes": false,
    "exit_node": "100.64.0.10"
  }]
}
```

This is an endpoint fragment, not a complete VPN configuration. Add a TUN inbound,
DNS rules, and routes selecting `tailnet` for the desired traffic. The native core
owns the Tailscale connection inside the same VPN service. Platform VPN limits
mean the design should not depend on another Tailscale VPN app running alongside it.

For unattended authentication, provide an appropriately scoped Tailscale auth key
through your application's secure configuration flow. The plugin does not yet
expose an interactive Tailscale login or endpoint-management API. Do not depend on
log messages as an authentication protocol. See the upstream
[Tailscale endpoint documentation](https://sing-box.sagernet.org/configuration/endpoint/tailscale/).

## Reproducible native builds

All Go compilation uses the Tailscale-maintained Go fork. The build refuses to
substitute a system Go toolchain. Inputs are fixed in [native-core.json](native-core.json):

- sing-box: `1.14.4`, revision `c005f35300ce76427dc285481ec5695f5b179cd2`.
- Tailscale Go: revision `63ae404c8203317fd3c82d972e5dc8f0fcb425cb`.
- SagerNet gomobile: `v0.1.12`.

From a source checkout:

```bash
python3 tool/build_libbox.py --target apple
```

```bash
python3 tool/build_libbox.py --target android
```

Or, from a Flutter app that depends on the package:

```bash
dart run sing_box_flutter build --target apple
```

Apple builds require macOS and Xcode. Android builds require a JDK, Android SDK,
and NDK. Toolchains and source caches live under `.build/` inside the package
checkout; they are excluded from Git and pub.dev. See [native build details](doc/native-build.md)
for prerequisites, verification, and artifact provenance.

## Example and development

The [example](example/) accepts editable sing-box JSON and demonstrates permission,
validation, connection status, and explicit disconnection. Its default configuration
captures traffic through a TUN and routes it directly; it does not connect to an
external proxy or Tailscale network automatically.

```bash
flutter pub get
```

```bash
flutter analyze
```

```bash
flutter test
```

Run the example's widget test from `example/`:

```bash
flutter test
```

```bash
flutter pub publish --dry-run
```

Native compilation and physical-device VPN tests are separate from Dart unit tests.
Before shipping an app, test DNS, IPv4/IPv6, lock/unlock, Wi-Fi/cellular transitions,
permission revocation, and UI restarts on the actual supported OS versions.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md), including the upstream core's
additional naming and association notice. Distributing an application that links
libbox carries the applicable source and notice obligations.
