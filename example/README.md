# sing_box_flutter example

Demonstrates configuration validation, system VPN consent, native connection
status, and explicit start/stop. Opening the app does not start a VPN.

The example selects a minimal WGC core in its `pubspec.yaml`. Build its native
artifact from source before building the app. Version 0.3.0 releases contain source
and build scripts, with no universal precompiled libbox `.a`, AAR or XCFramework
SDK assets. See [feature selection](../doc/core-features.md).

Install Python 3.11+ and Git, plus OpenJDK 17 and the Android SDK/NDK for Android, or
macOS with Xcode and CocoaPods for iOS. The native builder requires Android NDK
`28.0.13004108`; see [SDK setup](../doc/native-build.md#host-sdks-and-isolation) for
the separate Flutter packaging NDK requirement. The builder downloads and verifies
the pinned Tailscale Go toolchain automatically. No global Go installation is
needed on the build machine, and the installed app needs no separate Go
installation.

From this directory, resolve packages and build the platform you need:

```bash
flutter pub get
dart run sing_box_flutter build --target android
```

```bash
dart run sing_box_flutter build --target apple
```

```bash
dart run sing_box_flutter build --target windows --windows-arch amd64
```

Selected artifacts stay under this example's `.dart_tool/sing_box_flutter/`.
Rebuild the core after changing its pubspec selection, then rebuild the app.
`flutter pub get` and `flutter run` do not compile a missing core automatically.
Historical `install --release v0.2.0` artifacts are supported only for hosts without
a feature selection; they cannot satisfy this example's selected core.

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
After building the Android core with OpenJDK 17 and the Android SDK/NDK installed:

```bash
flutter pub get
```

```bash
flutter run
```

Run these from this example directory. The editable default configuration creates
a direct-routing VPN for testing, with no external proxy or Tailscale login.

## Windows (Wintun)

Install Visual Studio's Desktop development with C++ workload, the Windows SDK,
Python 3.11+, Git and Flutter. Build the selected Windows core, then package:

```bash
flutter build windows --release
```

Launch `build/windows/x64/runner/Release/sing_box_flutter_example.exe` and accept
the UAC prompt. The example's manifest requests administrator privileges for
Wintun adapter creation. Use an elevated terminal for `flutter run -d windows`.
CMake bundles the selected helper, which embeds sing-tun's verified official
Wintun driver, and retains its license under `sing_box_flutter/Legal/`. For ARM64,
build the core with `--windows-arch arm64` and Flutter with
`--target-platform windows-arm64`. See [Windows setup](../doc/windows-setup.md).

The example passes `dev.idy.singboxflutter.example` as its stable Windows
`applicationId`. Override `APPLICATION_ID` with your own unique identifier when
adapting the host; keep the same ID across versioned-directory updates. IDs are
case-sensitive and do not derive from executable paths.

Cancelling the status subscription leaves the tunnel connected. Explicit stop or
app exit closes the core and releases its routes; no Windows service is installed.

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
