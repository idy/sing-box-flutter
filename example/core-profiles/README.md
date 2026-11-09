# Example core profiles

The same Flutter app can exercise three native feature sets. These files contain
**build selections**, not server addresses, keys or runtime VPN configurations.
The checked-in `example/pubspec.yaml` defaults to `wgc-minimal`.

| Profile | Purpose |
| --- | --- |
| `wgc-minimal` | TUN, direct, WGC, UDP/TCP DNS and FakeIP; comparable in scope to wgc-ne |
| `wgc-tailscale` | Adds standard WireGuard, Tailscale, selectors/URL tests, DoH and Tailscale DNS |
| `singbox-full` | All currently supported common Android/iOS component types, proxy transports, TLS extensions and management APIs |

## Switch and build

Run from `example/`:

```bash
dart run tool/select_core_profile.dart wgc-minimal
```

```bash
dart run sing_box_flutter build --target apple
```

```bash
flutter run
```

Replace the first command's profile with `wgc-tailscale` or `singbox-full`. For an
Android app, build with `--target android`; add `--android-abis arm64-v8a` if only
an arm64 device is needed. Apple builds default to both device and simulator.

The selector validates a preset and replaces only the `sing_box_flutter` block in
the real example `pubspec.yaml`. It preserves app dependencies, version, Flutter
settings and host signing configuration. Native manifests become stale after a
selection change: rebuild the core and app. A successful Apple core build
invalidates only CocoaPods' generated sandbox receipt (not `Podfile.lock`), causing
Flutter to refresh pod integration on its next build. Direct Xcode builds must run
`pod install` first. Cached Pods also reject a changed profile explicitly. Hot
reload cannot change the native feature set. Previous content-addressed binaries
remain cached; a mismatched old artifact is never used silently.

## Measured core sizes

Using the pinned core/toolchain, Xcode 27.1 and Android NDK 28.0.13004108:

| Profile | iOS arm64 static library | iOS Release extension | Android arm64 native `.so` |
| --- | ---: | ---: | ---: |
| `wgc-minimal` | 26.60 MiB | 21.75 MiB | 22.96 MiB |
| `singbox-full` | 93.10 MiB | 78.54 MiB | 76.69 MiB |

These are per-architecture core binaries, not total app sizes, compressed downloads
or runtime memory. The full example includes additional modules beyond the previous
standard libbox mobile profile; it is intentionally larger. Intermediate profiles
share dependencies, so their sizes cannot be estimated by adding individual rows.

## What full means

`singbox-full` provides the broad mobile capability set expected from a full
libbox SDK: SOCKS/HTTP/Mixed and proxy inbounds, proxy outbounds, selector/URL-test,
all six supported VPN endpoint types, encrypted DNS, uTLS/REALITY/ECH, V2Ray
transports, Clash/V2Ray APIs, hosted services and certificate providers. Compared
with the repository's standard libbox mobile distribution, it also explicitly
selects OpenVPN/OpenConnect, Cloudflared, ACME, CCM/OCM and the V2Ray API.

It is not a claim that every upstream desktop/server facility works in a mobile
VPN extension. Linux Redirect/TProxy/systemd-resolved, native bridge/helper/Tor
facilities, USB/IP and Apple's incompatible Naive outbound are excluded. The Naive
inbound remains selectable and uses QUIC. See the
[complete upstream catalogue](../../doc/core-features.full.example.yaml) for those
additional names and [feature selection](../../doc/core-features.md) for limits.

Compiling a server/service type does not start a server. Only components referenced
by the runtime sing-box JSON run. Every profile retains `direct`, so the example's
default direct-routing JSON works without account credentials. To actually connect
WGC, Tailscale or a proxy, supply the corresponding runtime JSON and credentials;
selecting a profile neither authorizes an account nor establishes a VPN.
