# Native libbox builds

The Dart package ships source and tooling. Native AAR/XCFramework files and `.build/`
are excluded from the pub archive. Plugin users install a separately released core
before building their app; maintainers can reproduce it from the pinned sources.
Python 3.11 or newer is recommended (CI uses Python 3.13). No Python packages are
required. Run the commands from the plugin directory, including when using a local
copy of the pub package. Python must execute these checked-in scripts rather than
scripts found inside a downloaded archive.

## Install a published core

After a maintainer publishes a release containing the assets described below:

```sh
dart run sing_box_flutter install --target android --release v0.2.0
dart run sing_box_flutter install --target apple --release v0.2.0
# Equivalent direct invocation, using your Python 3.11+ executable:
python3 tool/install_libbox.py --target all --release v0.2.0
```

`--dry-run` prints URLs and destinations without writing files or downloading.
`--force` replaces a different existing artifact only after validating its replacement.
The installer uses only `https://github.com/idy/sing-box-flutter/releases/download/`
and requires an explicit tag; it does not select `latest`. If the release has not
been published, the command fails rather than manufacturing or compiling a binary.
The installer does not need Go, Xcode, an Android SDK, or Java.

The installer downloads `SHA256SUMS`, verifies each archive's SHA-256 before
extraction, rejects unsafe archive entries, checks the core/toolchain/mobile-profile
pins in `native-build.json`, and verifies all artifact contents. An AAR must contain
`classes.jar` and `AndroidManifest.xml`; an XCFramework must contain `Info.plist`.
Framework symlinks are hashed as `SHA256("symlink:" + link target)` and must resolve
within the artifact. Each download has a fresh directory under `.build/install`.
Installation provenance stays under `.build/install/installed/`. For artifacts
already downloaded from a CI run, use the same validator without network access:

```sh
python3 tool/install_libbox.py --target android --from-dir .build/ci-input/release
```

The directory must contain the archive and `SHA256SUMS`; `--from-dir` is mutually
exclusive with `--release`. Local archives are copied to a fresh private directory
before hashing/extraction, and pass the same pin/profile/contents checks.

The manifest and archive are fetched over HTTPS from the same trusted release.
This detects corruption but does not protect against a compromised release account.
For an independent trust anchor, obtain the manifest digest from the maintainer's
release announcement or verified CI summary and pass it explicitly:

```sh
python3 tool/install_libbox.py --target apple --release v0.2.0 \
  --manifest-sha256 <64-character-SHA256-of-SHA256SUMS>
```

## Build from source

All Go compilation uses **Tailscale's Go fork**. Stock Go is never selected as a
fallback, and `GOTOOLCHAIN=local` prevents Go from downloading another toolchain.
Inputs are read from `native-core.json`:

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

```sh
python3 tool/build_libbox.py --target all --dry-run
python3 tool/build_libbox.py --target apple
python3 tool/build_libbox.py --target android
# Download/verify inputs and build the pinned mobile tools without a native SDK:
python3 tool/build_libbox.py --target apple --bootstrap-only
```

The bootstrap checkout validates the official remote, exact tag-to-commit mapping,
`HEAD`, git object integrity, tracked changes, unexpected files, gomobile's module
pin, and required upstream build tags. Go modules use `go.sum`/`sum.golang.org`;
`go mod verify` checks cached module contents. Pinned `gomobile` and `gobind` are
rebuilt using the verified fork and inspected with `go version -m` before use.
Both tools and the core use project-local Go caches.

The `native/wgc` module is only a sing-box endpoint adapter; it directly depends on
`github.com/idy/wgc` for WireGuard sessions and encryption. `tool/wgc_overlay.py`
registers that adapter in the pinned libbox build and restores the upstream inputs
afterward. The adapter source and registration hashes are recorded in native
metadata so the installer cannot mix an unmodified libbox with a WGC-enabled
package. The bundled adapter accepts only the canonical files listed in
`tool/source_inputs.py`; extra inputs such as ignored `debug.log` files fail,
keeping checkout and pub-package identities identical. Extension fingerprints
include source files, assembly and embedded data, excluding VCS metadata and
reserved `.build`/`.wgc-inputs` generated state. The adapter and local extensions
compile from verified private snapshots under stable relative replacement paths.
Remote extensions require module and `go.mod` checksum pins in the manifest;
these are enforced in the main module even when private scopes disable sumdb.
After resolving extensions, the build checks
the selected Tailscale dependency's toolchain contract again and records its actual
version/checksums. Custom transport registrations can be compiled with `--wgc-config`; see
[WGC configuration](wgc.md). Exported `GOPRIVATE`, `GONOPROXY`, `GONOSUMDB`, and
`GOPROXY` settings are retained under the pinned compiler. Before dependency
resolution, both checked-in WGC module checksums are copied into the main module's
`go.sum` and conflicting entries are rejected, including when `GONOSUMDB` covers WGC.
Required WGC license text is collected from the pinned dependency for the native
release archive.

The official entry point remains `go run ./cmd/internal/build_libbox -target
android` or `-target apple -platform ios,iossimulator`. The tool applies the small,
deterministic feature profile in `tool/mobile-profile.json` while running it and
restores the pristine source in `finally`. The exact upstream builder SHA-256 is
checked before the edit. The profile removes only `with_naive_outbound`,
`with_usbip`, `with_openvpn`, and `with_openconnect`; these are outside this plugin's
feature contract. Upstream naive outbound pulls Cronet native libraries whose
pinned iOS archive referenced an undefined `base::MessagePumpKqueue::InitializeFeatures()`
and `UIApplication` symbols during real PacketTunnel linking. No upstream version,
API or dependency pin is changed. An interrupted process can leave the local
checkout dirty; remove `.build/bootstrap/sing-box` and retry in that case.

The profile preserves `with_tailscale`, `with_gvisor`, `with_wireguard`, `with_quic`,
`with_utls`, `with_clash_api`, the upstream linker compatibility tags, and all
upstream `ts_omit_*` mobile tags. Apple also preserves `with_low_memory` for non-macOS
builds, `with_dhcp`, and `grpcnotrace`. Exact effective tags and the profile/builder
hashes are recorded in build metadata. The upstream Android builder also emits its
legacy variant; the plugin installs only the SDK-24 main `libbox.aar`.

Outputs are:

- `android/libs/libbox.aar` (Android architectures selected by official gomobile).
- `ios/Frameworks/Libbox.xcframework` (iOS arm64 and simulator arm64/x86_64).
- `.build/cache/generated/android/libbox-sources.jar`, when produced upstream.
- `.build/cache/results/<target>.json`, with input pins, tag/profile hashes, SDK
  versions, download URL/checksum, module sums, binary identities and artifact hashes.
- `.build/release/libbox-1.14.4-<target>.tar.gz` and `SHA256SUMS`.

Metadata contains no machine absolute paths, environment dumps, or credentials.
Reproducibility means pinned inputs and recorded SDK versions, not a promise of
identical compiler output across different Xcode versions. Packaging normalizes
archive ordering, timestamps, owners and gzip headers.

## Host SDKs and isolation

Apple builds require macOS, Xcode command-line selection, and iPhoneOS plus
simulator SDKs. Check with `xcode-select -p`, `xcrun --sdk iphoneos
--show-sdk-version`, and `xcrun --sdk iphonesimulator --show-sdk-version`. The pinned
upstream builder requires iOS 15.0 or later. It is restricted to iOS/simulator;
macOS and tvOS frameworks are not built for this Flutter plugin.

Android builds require a licensed SDK in `ANDROID_HOME` (or `ANDROID_SDK_ROOT`),
installed platforms/build-tools, NDK `28.0.13004108`, and **OpenJDK 17** selected with
`JAVA_HOME`. The exact NDK requirement removes the upstream builder's permissive
fallback to a different NDK. Missing SDKs or Java cause a clear error. Local tooling
never installs or changes a global SDK. The Flutter plugin and example use NDK
`28.2.13676358` to package the prebuilt core and match Flutter's integration-test
plugin. CI installs both NDK versions; native Go compilation retains its recorded
`28.0.13004108` input. Flutter host compilation is a separate check.

`--bootstrap-dir` and `--cache-dir` accept only separate, non-nested directories
inside this project's `.build/`. Symlinks cannot redirect them outside the project.
Downloaded archives are isolated from one another and are never a working directory
for Python or build tools. A project lock prevents build/install races. Use the
same cache directory for a subsequent `--package-only` operation:

```sh
python3 tool/build_libbox.py --target apple --package-only
```

Cached Go archives are rehashed; cached modules are verified; edited core source is
rejected. To recover from a corrupt cache, remove the reported `.build/` subtree
and rerun. Do not replace it with a stock Go installation.

## Validation and release workflow

```sh
python3 -B -m unittest discover -s tests -v
python3 tool/build_libbox.py --target all --dry-run
python3 tool/install_libbox.py --target all --release v0.2.0 --dry-run
```

`.github/workflows/native-core.yml` validates the tooling for relevant PRs.
`workflow_dispatch` accepts `all`, `android`, or `apple`. It builds with verified
Tailscale Go, then compiles the real Flutter example/plugin against that artifact
(Android debug APK or iOS simulator including the PacketTunnel target). Android's
SDK is installed under the job's `.build/android-sdk`; the runner's JDK 17 is used.
The Android job also executes `TestAndroidUDPProtectionSmoke` on an API 35 x86_64
emulator using the same pinned fork. Controlled protection callbacks exercise
real Android UDP sockets, failure handling and reopening; this is not a VPN
forwarding or physical-device test.
PR and manually dispatched builds retain read-only repository permissions and
upload build logs, provenance, generated Android sources, and release inputs,
including on failure. With target `all`, a successful job combines both archive
checksums, validates the publication plan without publishing, and reports the
`SHA256SUMS` digest in the job summary.

Pushing a `vX.Y.Z` tag automatically builds and tests both platforms. The build
workflow is always read-only. A separate `Publish native artifacts` workflow runs
from the default branch via `workflow_run` and receives release-write permission.
Before downloading artifacts, its trusted gate checks the completed build's
workflow identity, repository, successful tag-push event and default-branch
ancestry. The publisher and imported Python helpers come from that independently
trusted checkout; tagged version/pin files are read as data and never executed.
The tag must match the built snapshot's `pubspec.yaml`; build metadata such as
`v0.2.1+build-prod` is supported by both publisher and installer.

PRs, forks and manual validation runs cannot publish. No personal token or local
download/re-upload is needed. The publisher verifies all uploaded sizes and
SHA-256 digests before making a draft public. Recovery preserves maintainer notes
and updates only its managed native-artifact section. Differing assets on an
already published release cannot be overwritten. Existing `v0.2.0` assets remain
unchanged. Dart publication to pub.dev is separate; these workflows distribute
the native libraries only.

To release a future version, update and merge the package version, then push its
matching version tag. A successful tag run publishes exactly these assets:

- `libbox-1.14.4-android.tar.gz`
- `libbox-1.14.4-apple.tar.gz`
- `SHA256SUMS` containing both archive digests

Each archive includes `native-build.json` and the upstream `LICENSE.sing-box`.
Keep upstream/dependency license notices and make the exact corresponding GPL source
and build profile available with the binary release. Do not put binaries, build
caches, or release archives in the pub package. Publish the manifest's digest in the
release notes so users can supply `--manifest-sha256`. Core assembly alone is not
proof that a VPN plugin links or works; device permission/tunnel behavior still
requires testing on actual Android and iOS devices.
