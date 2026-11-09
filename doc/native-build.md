# Native core builds

Version 0.3.0 ships source and build scripts. Hosts select features in their own
`pubspec.yaml` and compile the native core with the pinned Tailscale Go toolchain.
The GitHub Release has no universal precompiled libbox `.a`, AAR or XCFramework
SDK assets. The pub package also excludes native outputs and build caches while
retaining the sources and tools needed to build them.

Python 3.11+ and Git are required; no third-party Python packages are needed.
The builder downloads and verifies its Go compiler automatically, so a global Go
installation is unnecessary. The installed app needs no separate Go installation;
the Go runtime is compiled into its native core. Python remains necessary on the
build machine when Gradle or CocoaPods validates selected artifacts.

## Build a host-selected core

Add the package dependency and a top-level feature selection to the **Flutter
host application's** `pubspec.yaml`:

```yaml
dependencies:
  sing_box_flutter: ^0.3.0

sing_box_flutter:
  core:
    inbounds: [tun]
    outbounds: [direct]
    endpoints: [wgc]
    dns: [udp, tcp, fakeip]
```

Install the [platform SDK prerequisites](#host-sdks-and-isolation), then run from
that host app:

```sh
flutter pub get
dart run sing_box_flutter build --target android
```

```sh
dart run sing_box_flutter build --target apple
```

The selectable builder generates a small `native/mobile` binding package and only
the requested constructor registry, then invokes verified gomobile directly. It
does not use the upstream builder's fixed feature list or its RPC/profile-management
package. `--dry-run` inspects the selection without downloading or compiling.

Selected artifacts, hashes and capabilities stay under the host's
`.dart_tool/sing_box_flutter/`; the plugin's shared pub-cache artifacts are not
replaced. A host pubspec or source-profile change requires a core rebuild and a
native app rebuild. On iOS, rerun `pod install` or let Flutter run it. Normal
`flutter pub get`, Gradle and CocoaPods do not download Go or compile a missing
core. See [feature selection](core-features.md) and the checked-in example for
profiles, architecture choices and artifact validation.

The Dart CLI locates and runs the packaged Python scripts. Run host commands from
the app directory; direct `python3 tool/...` examples in this document run from the
plugin directory, including a local copy of the pub package. Python must execute
these checked-in scripts rather than scripts found inside a downloaded archive.

## Compiler verification and source inputs

All Go compilation uses **Tailscale's Go fork**, including selections without
Tailscale networking. Stock Go is never selected as a fallback, and
`GOTOOLCHAIN=local` prevents Go from downloading another toolchain. Inputs are read
from `native-core.json`:

| Input | Pin |
| --- | --- |
| sing-box | v1.14.4, commit `c005f35300ce76427dc285481ec5695f5b179cd2` |
| Tailscale Go | `63ae404c8203317fd3c82d972e5dc8f0fcb425cb` (`go1.26.5`) |
| Upstream mobile module | `github.com/sagernet/gomobile v0.1.12` |

`tool/tailscale-go.json` pins the official release archive SHA-256 for each supported
macOS/Linux arm64/amd64 host. These digests came from GitHub's asset `digest` values
for Tailscale's `build-63ae404c8203317fd3c82d972e5dc8f0fcb425cb` release, with the
source API URL recorded in the lock. The archive checksum is validated on every
invocation, then the toolchain is extracted again before any downloaded executable
runs. A modified cached executable or a local provenance stamp cannot substitute
for verification. `VERSION`, `go env`, explicit `GOROOT`, and the first `PATH` entry
must agree. The Tailscale dependency's embedded `go.toolchain.rev` / next-revision
contract is checked before native compilation; a mismatched fork fails early rather
than producing a binary that panics at startup. No mismatch environment override is
used. No source build of Go or globally installed Go is used.

To download/verify inputs and build the pinned mobile tools before installing a
native SDK, run from the configured host:

```sh
dart run sing_box_flutter build --target apple --bootstrap-only
```

The bootstrap checkout validates the official remote, exact tag-to-commit mapping,
`HEAD`, git object integrity, tracked changes, unexpected files, gomobile's module
pin, and required upstream build tags. Go modules use `go.sum`/`sum.golang.org`;
`go mod verify` checks cached module contents. Pinned `gomobile` and `gobind` are
rebuilt using the verified fork and inspected with `go version -m` before use.
Both tools and the core use project-local Go caches.

The `native/wgc` module is only a sing-box endpoint adapter; it directly depends on
`github.com/idy/wgc` for WireGuard sessions and encryption. `tool/wgc_overlay.py`
registers that adapter when WGC is compiled and restores the upstream inputs
afterward. The adapter source and registration hashes are recorded in native
metadata. The bundled adapter accepts only the canonical files listed in
`tool/source_inputs.py`; extra inputs such as ignored `debug.log` files fail,
keeping checkout and pub-package identities identical. Extension fingerprints
include source files, assembly and embedded data, excluding VCS metadata and
reserved `.build`/`.wgc-inputs` generated state. The adapter and local extensions
compile from verified private snapshots under stable relative replacement paths.
Remote extensions require module and `go.mod` checksum pins in the manifest;
these are enforced in the main module even when private scopes disable sumdb.
After resolving extensions, the build checks the selected Tailscale dependency's
toolchain contract again and records its actual version/checksums. Custom transport
registrations can be compiled with `--wgc-config`; see [WGC configuration](wgc.md).
Exported `GOPRIVATE`, `GONOPROXY`, `GONOSUMDB`, and `GOPROXY` settings are retained
under the pinned compiler. Before dependency resolution, both checked-in WGC module
checksums are copied into the main module's `go.sum` and conflicting entries are
rejected, including when `GONOSUMDB` covers WGC. Required upstream/dependency
license notices are retained with native build outputs.

Metadata contains no machine absolute paths, environment dumps, or credentials.
Reproducibility means pinned inputs and recorded SDK versions, not a promise of
identical compiler output across different Xcode versions.

## Host SDKs and isolation

Apple builds require macOS, Xcode command-line selection, and iPhoneOS plus
simulator SDKs. Check with `xcode-select -p`, `xcrun --sdk iphoneos
--show-sdk-version`, and `xcrun --sdk iphonesimulator --show-sdk-version`. The minimum
iOS deployment target is 15.0. Builds are restricted to iOS/simulator; macOS and
tvOS frameworks are not built for this Flutter plugin. The host also needs
CocoaPods and its own signed PacketTunnel target; see [iOS setup](ios-setup.md).

Android builds require a licensed SDK in `ANDROID_HOME` (or `ANDROID_SDK_ROOT`),
installed platforms/build-tools, NDK `28.0.13004108`, and **OpenJDK 17** selected with
`JAVA_HOME`. The exact NDK requirement removes the upstream builder's permissive
fallback to a different NDK. Missing SDKs or Java cause a clear error. Local tooling
never installs or changes a global SDK. The Flutter plugin and example use NDK
`28.2.13676358` to package the compiled core and match Flutter's integration-test
plugin. CI installs both NDK versions; native Go compilation retains its recorded
`28.0.13004108` input. Flutter host compilation is a separate check.

Selected-core builds keep the verified toolchain, source checkout and Go caches in
a native work directory, by default the plugin's `.build/`. When the resolved
package is read-only, such as a prepopulated pub cache, the builder uses the
host's `.dart_tool/sing_box_flutter/work/` instead; that repeats toolchain and
module downloads for each host. `--work-dir <absolute path>` selects a shared
writable directory. It must be empty or an existing sing_box_flutter work
directory, and outside the plugin sources.

`--bootstrap-dir` and `--cache-dir` accept only separate, non-nested directories
inside the work directory (the plugin's `.build/` for legacy standard builds).
Symlinks cannot redirect them outside it.
Downloaded archives are isolated from one another and are never a working directory
for Python or build tools. A project lock prevents build/install races. Selected
artifacts stay in the host's `.dart_tool/sing_box_flutter/`, separate from these
shared source/toolchain caches.

Cached Go archives are rehashed; cached modules are verified; edited core source is
rejected. To recover from a corrupt cache, remove the reported `.build/` subtree
and rerun. Do not replace it with a stock Go installation.

## Legacy standard-core build and install

Hosts without a `sing_box_flutter.core` block retain the historical standard
build/install path. This compatibility path does not provide 0.3.0 SDK downloads
and cannot satisfy a custom host selection.

### Build the standard core from source

Without a host selection, `dart run sing_box_flutter build` invokes the retained
standard helper. From a plugin source checkout, use it directly:

```sh
python3 tool/build_libbox.py --target all --dry-run
python3 tool/build_libbox.py --target apple
python3 tool/build_libbox.py --target android
```

The standard helper invokes the upstream `go run ./cmd/internal/build_libbox
-target android` or `-target apple -platform ios,iossimulator` entry point. It
applies the deterministic feature profile in `tool/mobile-profile.json` while
running it and restores the pristine source in `finally`. The exact upstream
builder SHA-256 is checked before the edit. The profile removes only
`with_naive_outbound`, `with_usbip`, `with_openvpn`, and `with_openconnect`.
Upstream naive outbound pulls Cronet native libraries whose pinned iOS archive
referenced undefined `base::MessagePumpKqueue::InitializeFeatures()` and
`UIApplication` symbols during real PacketTunnel linking. No upstream version,
API or dependency pin is changed. An interrupted process can leave the local
checkout dirty; remove `.build/bootstrap/sing-box` and retry in that case.

The profile preserves `with_tailscale`, `with_gvisor`, `with_wireguard`, `with_quic`,
`with_utls`, `with_clash_api`, the upstream linker compatibility tags, and all
upstream `ts_omit_*` mobile tags. Apple also preserves `with_low_memory` for non-macOS
builds, `with_dhcp`, and `grpcnotrace`. Exact effective tags and the profile/builder
hashes are recorded in build metadata. The upstream Android builder also emits its
legacy variant; the plugin installs only the SDK-24 main `libbox.aar`.

Standard outputs are:

- `android/libs/libbox.aar` (Android architectures selected by official gomobile).
- `ios/Frameworks/Libbox.xcframework` (iOS arm64 and simulator arm64/x86_64).
- `.build/cache/generated/android/libbox-sources.jar`, when produced upstream.
- `.build/cache/results/<target>.json`, with input pins, tag/profile hashes, SDK
  versions, download URL/checksum, module sums, binary identities and artifact hashes.
- `.build/release/libbox-1.14.4-<target>.tar.gz` and `SHA256SUMS`, for local or internal
  CI validation. The directory name does not make these 0.3.0 release assets.

Packaging normalizes archive ordering, timestamps, owners and gzip headers. Each
archive includes `native-build.json`, `LICENSE.sing-box` and applicable dependency
notices. To repackage verified cached standard outputs, use the same cache directory:

```sh
python3 tool/build_libbox.py --target apple --package-only
```

### Install a historical binary release

The retained installer supports existing releases such as `v0.2.0`. Run from a
host with no feature selection:

```sh
dart run sing_box_flutter install --target android --release v0.2.0
dart run sing_box_flutter install --target apple --release v0.2.0
```

From the plugin directory, the equivalent direct invocation is:

```sh
python3 tool/install_libbox.py --target all --release v0.2.0
```

`--dry-run` prints URLs and destinations without writing files or downloading.
`--force` replaces a different existing artifact only after validating its replacement.
The installer uses only `https://github.com/idy/sing-box-flutter/releases/download/`
and requires an explicit tag; it does not select `latest`. A release without the
required assets fails rather than manufacturing or compiling a binary. The
installer requires Python 3.11+, but does not need Go, Xcode, an Android SDK or Java.

The installer downloads `SHA256SUMS`, verifies each archive's SHA-256 before
extraction, rejects unsafe archive entries, checks the core/toolchain/mobile-profile
pins in `native-build.json`, and verifies all artifact contents. An AAR must contain
`classes.jar` and `AndroidManifest.xml`; an XCFramework must contain `Info.plist`.
Framework symlinks are hashed as `SHA256("symlink:" + link target)` and must resolve
within the artifact. Each download has a fresh directory under `.build/install`.
Installation provenance stays under `.build/install/installed/`.

For standard artifacts already downloaded from an internal CI run, use the same
validator without network access:

```sh
python3 tool/install_libbox.py --target android --from-dir .build/ci-input/release
```

The directory must contain the archive and `SHA256SUMS`; `--from-dir` is mutually
exclusive with `--release`. Local archives are copied to a fresh private directory
before hashing/extraction, and pass the same pin/profile/contents checks.

The manifest and archive are fetched over HTTPS from the same historical release.
This detects corruption but does not protect against a compromised release account.
For an independent trust anchor, obtain the manifest digest from the maintainer's
release announcement or verified CI summary and pass it explicitly:

```sh
python3 tool/install_libbox.py --target apple --release v0.2.0 \
  --manifest-sha256 <64-character-SHA256-of-SHA256SUMS>
```

## Validation and source-only release workflow

Run the tooling checks from the plugin directory:

```sh
python3 -B -m unittest discover -s tests -v
python3 tool/build_libbox.py --target all --dry-run
python3 tool/install_libbox.py --target all --release v0.2.0 --dry-run
flutter pub publish --dry-run
```

`.github/workflows/native-core.yml` validates relevant PRs and supports manual
Android/Apple builds. It builds with verified Tailscale Go, checks representative
selected registries and compiles the real Flutter example/plugin (Android debug
APK or iOS simulator including the PacketTunnel target). Runtime smoke checks and
native lifecycle tests complement compilation. Android uses a job-local SDK and
JDK 17. These checks do not replace physical-device VPN forwarding tests.

CI keeps read-only repository permissions. The `native-core-android`,
`native-core-apple` and `native-core-release` artifacts retain logs, provenance,
selected cores and standard archives as temporary compatibility-validation inputs.
In particular, `.github/workflows/android-plugin.yml` can consume a completed Native
core run and validate its standard archive with `--from-dir`, then restore a
compatible selected example core or compile one. These internal archives are not a
public SDK publication channel.

The Native core workflow has no version-tag push trigger or tag-only validation
step. The former `.github/workflows/publish-native.yml` publisher has been removed.
Pushing a version tag does not publish native binaries; 0.3.0 has no automatic
trusted native-artifact publisher.

A 0.3.0 GitHub Release describes the source build workflow and provides the tagged
sources/build scripts through GitHub's source archives. Do not attach universal
precompiled libbox static libraries, AARs, XCFrameworks or their SDK archives and
checksum manifests. Existing historical release assets such as `v0.2.0` remain
available for the legacy installer.

Before publishing the Dart package, inspect `flutter pub publish --dry-run` to
ensure it retains `native/mobile/`, the complete canonical `native/wgc/` adapter
(including its module/checksum files), `tool/`, `native-core.json` and required
license/notice files. Exclude `.build/`, `.dart_tool/`, generated native outputs,
AARs, static libraries, XCFrameworks and CI archives. A source package without its
build inputs cannot support this workflow. pub.dev publication remains separate
from creating the GitHub Release.

Keep upstream/dependency notices and make the exact corresponding GPL source and
build profile available when distributing an app that links the native core.
Before shipping, test permission/tunnel behavior, DNS, IPv4/IPv6, sleep/wake and
network transitions on actual Android and iOS devices.
