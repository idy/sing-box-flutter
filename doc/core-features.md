# Selectable native core

Version 0.3.0 uses source builds selected by the **host Flutter application's**
`pubspec.yaml`. Add `sing_box_flutter: ^0.3.0` under `dependencies` and place the
`sing_box_flutter.core` block at the top level of that same host manifest. The
GitHub Release provides source and build scripts, with no universal precompiled
libbox `.a`, AAR or XCFramework SDK assets.

Build-time selections control the available component types; normal sing-box JSON
still controls nodes, credentials, routing rules and DNS policy at runtime. New
protocols require rebuilding the native core and app; changing servers, keys or
rules does not.

## Minimal WGC build

```yaml
sing_box_flutter:
  core:
    inbounds: [tun]
    outbounds: [direct]
    endpoints: [wgc]
    dns: [udp, tcp, fakeip]
```

The checked-in example uses this selection. Ready-to-build
[WGC minimal, WGC + Tailscale and full mobile profiles](../example/core-profiles/README.md)
can be applied with its profile switcher, which changes only the real host pubspec's
feature block. It retains TUN, the gVisor userspace
stack, direct connections, WGC and DNS without compiling the entire upstream
mobile SDK. WGC's encrypted sessions remain the pinned `github.com/idy/wgc`
dependency; the protocol is not reimplemented here.

Run from the host app:

```bash
dart run sing_box_flutter build --target apple
```

```bash
dart run sing_box_flutter build --target android
```

```bash
dart run sing_box_flutter build --target windows --windows-arch amd64
```

Windows shares the feature selection and constructor registry, with a native
Wintun helper instead of the mobile binding. Use `--windows-arch arm64` for ARM64;
architecture is part of the Windows artifact identity. See
[Windows setup](windows-setup.md) for packaging and administrator requirements.

Use `--dry-run` to inspect the selection, resolved tags, fingerprint and output
receipt without downloading Go or compiling. The CLI reads the canonical host
pubspec bytes once and passes their digest and features through a private,
per-invocation request; edits during discovery or compilation require a fresh
build. `--config` must resolve to the actual host `pubspec.yaml`, not a separate
feature-only file. `--config /path/to/pubspec.yaml`
explicitly selects a host manifest; otherwise the CLI finds the nearest pubspec
from the caller's directory. A `pubspec.yaml` inside a dependency is never silently
substituted for the host manifest.

macOS uses `--target macos` and builds a universal arm64/x86_64 XCFramework for
macOS 12+. Select the backend in the same pubspec with
`sing_box_flutter.macos.mode: ne` (default) or `user_tun`. A macOS-only block can
omit `core` and use the normal feature defaults for `build --target macos`, while
Android/iOS keep their legacy artifacts. With only a `macos` block, use explicit
targets: `all` or an omitted target is rejected rather than silently changing the
mobile core. An explicit `core` block remains a global source selection. The mode
is part of the selected build fingerprint; switch it by rebuilding the core and
app. See [macOS setup](macos-setup.md).

The `apple` target defaults to device arm64 plus arm64/x86_64 simulators. Use
`--apple-platform ios` for a device-only SDK; that artifact cannot build simulator
apps. Android defaults to all four supported ABIs; use
`--android-abis arm64-v8a` for an arm64-only SDK. Architecture choices and SDK
versions are recorded separately from the logical feature selection. An AAR or
XCFramework containing several architectures is not the final installed size of
an app on one device.

Build prerequisites are Python 3.11+ and Git, plus OpenJDK 17 and Android SDK/NDK
for Android, or macOS/Xcode and CocoaPods for iOS. See
[native-build.md](native-build.md#host-sdks-and-isolation) for exact SDK requirements.
The builder automatically downloads and verifies the pinned Tailscale Go fork,
including for builds that do not enable Tailscale networking. No global Go
installation is needed on the build machine, and the installed app needs no
separate Go installation. Normal `flutter pub get`, Gradle and CocoaPods do not
silently download or compile a Go core.

## Selection rules

Each category is an explicit list of type names. Missing `inbounds`, `outbounds`
and `dns` default to `[tun]`, `[direct]` and `[local, tcp, udp]`; other missing categories
default to empty lists. An explicit `[]` stays empty. A mobile VPN build requires
`tun`. Unknown keys, unknown names, duplicate names, malformed lists and platform
incompatibilities are errors, not requests to build a different preset.

The [full catalogue](core-features.full.example.yaml) lists all upstream registered
component types plus WGC. It deliberately includes desktop/server facilities and
is not a portable mobile preset. Removed-protocol error stubs are not features.

| Category | Meaning |
| --- | --- |
| `inbounds` | Traffic listeners, such as TUN, HTTP, SOCKS, or proxy server protocols |
| `outbounds` | Direct/proxy connections and selector/URL-test groups |
| `endpoints` | WireGuard, Tailscale, OpenConnect, OpenVPN client/server, and WGC |
| `dns` | UDP/TCP, DoT/DoH/DoQ/HTTP3, FakeIP, hosts/local and endpoint DNS transports |
| `transports` | V2Ray HTTP, WebSocket, QUIC, gRPC and HTTPUpgrade transports |
| `tls` | Additional uTLS, REALITY and ECH capabilities |
| `api` | Experimental Clash/V2Ray compatibility APIs |
| `services` | Explicitly hosted services; separate from the mobile lifecycle controller |
| `certificate_providers` | ACME, Tailscale and Cloudflare Origin CA provisioning |

The five official endpoint names at the pinned 1.14.4 revision are `wireguard`,
`tailscale`, `openconnect`, `openvpn-client`, and `openvpn-server`. `wgc` is this
plugin's extension. In particular, WireGuard is no longer an outbound at this pin.

### Add Tailscale and selected proxy clients

```yaml
sing_box_flutter:
  core:
    inbounds: [tun]
    outbounds: [direct, selector, urltest, shadowsocks, vless, hysteria2]
    endpoints: [wgc, wireguard, tailscale]
    dns: [udp, tcp, https, fakeip, tailscale]
    transports: [ws]
    tls: [utls, reality]
```

Including Tailscale does not require running a DERP server. Including a client
protocol does not register its server-side inbound. OpenVPN client and server
constructors are registered separately. Selecting the Tailscale DNS/certificate
backend does not automatically expose a Tailscale endpoint type.

### Dependency and platform limits

The builder resolves tags for QUIC protocols/transports, uTLS/REALITY, userspace
stacks, optional VPN endpoints and services. Tailscale retains the upstream mobile
`ts_omit_*` profile for SSH, Taildrop, web clients and unrelated integrations.
Selecting the Tailscale endpoint does not enable those omitted subsystems.

Linux transparent interception (`redirect`/`tproxy`) and systemd-resolved integration
are unavailable through these Android/iOS/macOS hosts. Native bridge/helper-process and
USB/IP selections also fail when their required host facilities are unavailable.
Apple Naive outbound is rejected because the pinned Cronet archive has unresolved
symbols in a real NetworkExtension link. OpenVPN/OpenConnect have selectable tags;
source compilation is not evidence of signed-device VPN acceptance.

Some upstream modules share implementation packages. In particular, several V2Ray
transports are compiled together; ECH is part of the pinned Go TLS implementation,
not an effective upstream build-tag size toggle. The selected capability contract
is enforced, but deleting one of those entries does not promise removal of every
shared byte. Constructor selection removes unrelated engine registrations and the
large official RPC/profile-management binding layer. Measure actual linked output
for each profile instead of adding estimated per-feature sizes.

## Measured minimal WGC build

On 2026-11-10, with sing-box 1.14.4, the pinned Go fork, Xcode/iOS SDK 27.1
(targeting iOS 15) and Android NDK 28.0.13004108:

| Comparable artifact | Cached standard core | Selected WGC core |
| --- | ---: | ---: |
| iOS arm64 static library | 74.11 MiB | 26.61 MiB |
| iOS Release PacketTunnel executable | 59.94 MiB | 21.83 MiB |
| Android arm64 native `.so`, uncompressed | 60.58 MiB | 22.96 MiB |

The selected host uses exactly the minimal WGC block above. The cached standard
baseline predates WGC registration; the smaller selected build includes WGC.
The main iOS Release executable is 0.41 MiB and contains no Go runtime symbols,
compared with 59.93 MiB when it also linked the complete engine. These executable
sizes exclude other app resources and the Flutter framework; they are not IPA/APK
download sizes or RAM measurements. The arm64-only selected Android AAR is 8.23 MiB
compressed, not directly comparable to the standard four-ABI AAR.

Framework compilation, the real iOS Release app/extension link and simulator
version/fingerprint/configuration checks passed. Representative generated direct,
Tailscale/WireGuard, proxy-client, OpenVPN/OpenConnect and optional-service profiles
also compiled and ran their host lifecycle/capability tests. This does not establish
real-device VPN forwarding or every protocol's network behavior. Reproduce registry
checks with `python3 tool/check_core_profiles.py --target apple --race` (or target
`android` for that platform's feature constraints).

## Project isolation and provenance

Selected artifacts are immutable, content-addressed outputs under the host's
`.dart_tool/sing_box_flutter/builds/`. `native.json` is an atomic current-profile
receipt containing the selection, source fingerprint, version, pubspec hash,
artifact hashes and capabilities manifest paths. Both native build systems reject
missing, modified or stale artifacts. A changed profile drops pointers to another
profile's platform binaries; build each required target again.

Selected native builds perform a read-only Python recipe check against the
currently resolved plugin's native sources, tooling and pins. Python must remain
available when Gradle or CocoaPods consumes selected cores; set `SINGBOX_PYTHON`
if the interpreter is not discoverable. This validation does not download tools or
compile Go, and legacy standard-artifact builds retain their previous path.

The JNI/Go binary embeds the same fingerprint as its bundled capabilities manifest.
Android checks it during initialization; the iOS extension checks it before core
startup. Go compiler/module verification and WGC extension checksum rules remain
in effect. Local transport extensions use the existing `--wgc-config` manifest.

Different host apps never overwrite each other's native artifacts in the shared
pub cache. Build caches/toolchains remain in the plugin checkout's ignored `.build`.
The pub package retains the `native/mobile/`, `native/wgc/` and `tool/` sources and
pins needed for source builds, while excluding these caches and generated binaries.

Hosts without a `sing_box_flutter.core` block retain the legacy standard source
builder and installer for historical releases such as `install --release v0.2.0`.
Version 0.3.0 does not publish standard native SDK assets. Historical standard
assets cannot satisfy a custom selection; the CLI rejects `install` for configured
hosts instead of installing a larger or incompatible artifact silently. Custom
profiles are not published by this command. See
[legacy build and install details](native-build.md#legacy-standard-core-build-and-install).

## Add-to-app Flutter modules

Build the core from the Flutter module containing the selected pubspec. Android
can discover the generated `:flutter` module root. For custom layouts, set the
absolute `SINGBOX_HOST_PROJECT` environment variable or the
`singBoxFlutterProject` Gradle property to that module directory.

In a native iOS Podfile, set the module root before installing Flutter/plugin pods:

```ruby
ENV['SINGBOX_HOST_PROJECT'] = File.expand_path('../flutter_feature', __dir__)
```

The explicit root must contain `pubspec.yaml`. Without an override, ordinary
Flutter apps keep their existing project-root discovery. Changing module roots
requires refreshing CocoaPods and rebuilding the host.

## iOS validation and process ownership

Only the PacketTunnel extension links the Go engine. The Flutter app links shared
Swift storage/control and a bundled capabilities manifest. This avoids including
a second complete engine just to retrieve a version or inspect a configuration.

`checkConfig` on Android uses native engine validation. On iOS it checks JSON
structure and compiled feature compatibility in the app, without connecting or
requiring VPN consent. The extension performs full native validation and engine
construction before reporting connected. A successful iOS preflight therefore
cannot guarantee startup: endpoint keys, constructor-specific options and
platform resources may still fail. Startup errors remain observable through the
status stream and never contain raw config or credentials.

The selected engine preserves the pinned mobile SDK's implicit enabled cache,
including selector persistence and existing cache restoration. Custom cache paths,
IDs and storage flags remain configurable; as in that SDK, `enabled: false` does
not disable this mobile cache.

The selected mobile core does not forward raw engine diagnostics, which can contain
credential-bearing URLs. An explicitly requested private log file receives fixed
lifecycle messages only; stdout/stderr log targets are rejected. Log destinations
must stay beneath the host's private runtime Working directory. Traversal, symlink
escapes, hard-link aliases and non-regular files are rejected before permissions
or contents are changed. This differs from standard upstream protocol logging and
should be considered when diagnosing a connection.

Hot reload does not replace native binaries or the capabilities bundle. After a
core or pubspec change, rebuild the core and app. Apple core builds invalidate the
generated `Pods/Manifest.lock`, preserving `Podfile.lock`, so Flutter runs pod
installation again. Direct Xcode builds require `pod install`; cached pod phases
reject a changed source profile instead of linking an old framework silently.
Device permission handling, real IPv4/IPv6/DNS forwarding, sleep/wake, network
transitions and Tailscale/WGC sessions still require signed-device testing.
