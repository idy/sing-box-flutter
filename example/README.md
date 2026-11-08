# sing_box_flutter example

Demonstrates configuration validation, system VPN consent, native connection
status, and explicit start/stop. Opening the app does not start a VPN.

Install or build the native core first; see the [plugin README](../README.md).

## Android

The project includes the merged plugin service declaration and targets API 24+.
With a JDK and Android SDK/NDK installed:

```bash
flutter pub get
```

```bash
flutter run
```

Run these from this example directory. The editable default configuration creates
a direct-routing VPN for testing, with no external proxy or Tailscale login.

## iOS

The checked-in Xcode project contains an embedded PacketTunnel target and uses
CocoaPods. Select your own Apple team and replace the example bundle IDs and App
Group with values available to that team. Update entitlements in Runner/ and
PacketTunnel/ together. Follow [the iOS host guide](../doc/ios-setup.md).

From this directory:

```bash
flutter run --dart-define=APP_GROUP=group.dev.idy.singboxflutter.example --dart-define=TUNNEL_BUNDLE_ID=dev.idy.singBoxFlutterExample.PacketTunnel
```

A physical device is required for VPN traffic. Compile-only simulator verification:

```bash
flutter build ios --simulator --no-codesign
```

If regenerating the Xcode project, `ios/configure_project.rb` recreates the extension
target using the `xcodeproj` Ruby gem (included with CocoaPods). Do not add Flutter
to the PacketTunnel target; it depends only on the plugin's PacketTunnel subspec.
