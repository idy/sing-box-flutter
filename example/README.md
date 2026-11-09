# sing_box_flutter example

Demonstrates configuration validation, system VPN consent, native connection
status, and explicit start/stop. Opening the app does not start a VPN.

The example selects a minimal WGC core in its `pubspec.yaml`. Build its native
artifact before building the app; standard full-core release assets do not match
this selection. See [feature selection](../doc/core-features.md). From this directory:

```bash
flutter pub get
```

```bash
dart run sing_box_flutter build --target apple
```

For Android, replace the target with `android`.

## Choose the core feature set

Three ready-to-build profiles share this example app:

| Profile | Compiled capabilities |
| --- | --- |
| [wgc-minimal](core-profiles/wgc-minimal.yaml) | TUN, WGC, direct, basic DNS and FakeIP |
| [wgc-tailscale](core-profiles/wgc-tailscale.yaml) | WGC, WireGuard, Tailscale, selectors and encrypted DNS |
| [singbox-full](core-profiles/singbox-full.yaml) | Full supported mobile protocol/DNS/TLS/API/service set, comparable to a complete libbox mobile SDK |

From this directory, choose one before building the native core:

```bash
dart run tool/select_core_profile.dart singbox-full
```

```bash
dart run sing_box_flutter build --target apple
```

Use `wgc-minimal` for the smallest WGC build, or `wgc-tailscale` for the intermediate
profile. Use target `android` for Android. The selector updates only the feature
block in the real pubspec; changing it requires rebuilding the core and app. The
full profile deliberately excludes unavailable desktop/helper facilities and
Apple's incompatible Naive outbound; see [profile details](core-profiles/README.md).

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
