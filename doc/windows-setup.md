# Windows and Wintun

The Windows backend runs a host-selected sing-box core in a bundled helper
process. sing-box creates the native TUN interface with the official Wintun DLL;
Flutter uses the same `SingBoxFlutter` API as Android and iOS. Windows 10 or newer
is required. Core builds support x64 (`amd64`) and ARM64 (`arm64`); the Flutter
runner and the core must have matching architectures.

## Build the selected core

Install Flutter 3.47.2+, Python 3.11+, Git, and Visual Studio's **Desktop development
with C++** workload, including the Windows SDK and CMake 3.19+ (the bundled Visual
Studio version is sufficient). Packaging defers resource installation rules until
the host has selected its final install prefix. Flutter's Windows packaging runs
on Windows. The Go core can also be cross-compiled on macOS/Linux.

Add `sing_box_flutter.core` to the host app's pubspec, for example:

```yaml
sing_box_flutter:
  core:
    inbounds: [tun]
    outbounds: [direct]
    endpoints: [wgc]
    dns: [udp, tcp, fakeip]
```

Run from the Flutter host app:

```bash
flutter pub get
```

```bash
dart run sing_box_flutter build --target windows --windows-arch amd64
```

```bash
flutter build windows --release
```

For an ARM64 host, select `--windows-arch arm64` when building the core and
`--target-platform windows-arm64` when building Flutter. No system Go installation
or MinGW compiler is required. The builder downloads and verifies the pinned
Tailscale Go compiler and selected Go dependencies. The pinned sing-tun module
embeds the official Wintun 0.14.1 driver for each architecture and loads it through
its in-memory loader. Before compiling, the builder verifies the selected module
path, version and checksum, then checks the embedded resource against
`tool/wintun.json`. It never downloads a separate Wintun ZIP or bundles a sidecar DLL.

Windows requires a host feature selection; the historical mobile `install`
command cannot provide a Windows core. `--target all` retains its Android + Apple
meaning, so build Windows explicitly.

Artifacts stay in the host's `.dart_tool/sing_box_flutter/`, with a receipt bound
to the host pubspec, selected features, native source, compiler pins, Wintun hashes,
and architecture. The Windows pointer is `windows.json`; mobile cores retain their
separate `native.json`, so building one platform does not replace the other's
active artifact. Windows artifacts use
`builds/windows/<combined-sha256>/WindowsCore`, where the full digest binds the
recipe fingerprint and all artifact file hashes. Full recipe and file identities
remain in the receipt and build metadata. CMake validates the path and digest in
one read-only invocation, then rechecks the configured pair before every build.
It bundles `sing_box_flutter_core.exe` beside the host executable and installs
capabilities, build provenance, and notices under `sing_box_flutter/`. The exact
official driver license is package-owned at `native/windows/LICENSE.wintun` and
retained under `sing_box_flutter/Legal/LICENSE.wintun` in the app.
Keep the complete Flutter release directory together when distributing the app.
Rebuild the core after changing its feature selection or updating the plugin.
A normal Flutter build reports a missing/stale core instead of downloading one.

## Administrator privileges

Creating a Wintun adapter and changing routes requires an elevated application.
Secure storage initialization also requires elevation and fails before creating
managed state when the helper is not elevated. `requestPermission()` checks the
helper's inherited process token; `start()` also enforces elevation. These methods
do not relaunch the app or prompt for UAC. The elevated token must already hold
`SeSecurityPrivilege` for atomic protected-SACL creation and opening the root for
full SACL inspection. The helper enables this right only on a temporary thread
token around those opens; it does not grant missing rights or modify the process
token or machine security policy.

Either launch the built host with **Run as administrator**, or add the following
to the host's `windows/runner/runner.exe.manifest`:

```xml
<trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
  <security>
    <requestedPrivileges>
      <requestedExecutionLevel level="requireAdministrator" uiAccess="false"/>
    </requestedPrivileges>
  </security>
</trustInfo>
```

When the input manifest supplies the UAC level, suppress MSVC's default
`asInvoker` fragment in the host's `windows/runner/CMakeLists.txt`, after the
executable target is defined:

```cmake
if(MSVC)
  target_link_options(${BINARY_NAME} PRIVATE "/MANIFESTUAC:NO")
endif()
```

The supplied `requireAdministrator` manifest remains embedded in the executable.
The checked-in example includes both settings, and Windows CI verifies the final
embedded elevation level. Launch its built executable to accept the Windows UAC
prompt. For `flutter run`, use an elevated terminal so
Flutter can launch the runner. A plugin cannot change the host's manifest.

## Configuration and lifecycle

Use complete sing-box JSON with a `tun` inbound, `auto_route: true`, and
`route.auto_detect_interface: true` to avoid routing the outbound connection back
into the tunnel. The example's direct-routing configuration works on Windows.
`stack: "gvisor"` is available when the selected core includes TUN.

Windows initialization requires `applicationId`, a unique, case-sensitive host
identifier such as `dev.example.vpn`. Keep that exact ID in every application
version, including updates that change the executable directory. Independently
operated applications must use different IDs. Android and iOS ignore this option.

`initialize()` creates application storage under
`%LOCALAPPDATA%/v1-<short application ID>-<application ID SHA-256>/sing_box_flutter`.
The fixed `v1-` prefix identifies the secure storage format and stays constant
across application updates. Executable paths and filename case do not determine
the namespace. Changing the ID intentionally selects separate empty storage.
Earlier unreleased path-based or in-place-sealed storage is left untouched and
is not automatically adopted.

Storage must be on a local volume with persistent ACL support and no reparse-point
ancestors. The helper creates managed objects atomically with private ACLs and
no-write-up labels, and retains verified directory and lease handles until all
core/check cleanup completes. Existing objects are verified without rewriting
their security descriptors: private single-link state with the required integrity
label keeps its contents across restarts. Unsafe ACLs, insufficient integrity,
reparse points and multi-linked state cause initialization to fail without
repairing or adopting those objects. Directory entries are checked by file
identity, including distinct names in case-sensitive directories. Administrator
privileges are required to create or reopen storage; normal cache and temporary-file
features remain enabled.
The Apple identifiers and Android notification title are not used on Windows. `checkConfig()` parses against
the selected native registry and validates the engine before connecting.
Configurations travel through an anonymous process pipe, never command-line
arguments or a temporary config file. Native protocol diagnostics are suppressed;
platform errors and status events use fixed messages. Encoded UTF-8 requests,
including JSON escaping and newline framing, must fit within 8 MiB. Oversized
requests return `request_too_large` locally without closing an existing VPN.

`start()` acknowledges an accepted request. Observe the status stream for
`connecting`, `connected`, or `error`. Cancelling subscriptions and rebuilding a
widget do not stop the VPN. Call `stop()` to close sing-box, restore routes, and
release Wintun. The helper closes the core when its control pipe reaches EOF,
including when the app exits. Unlike the mobile system services, Windows does not
install a persistent background service: the tunnel belongs to the running app.
After helper failure, `getStatus()` returns the cached failure and `stop()` waits
for the old process to finish cleanup without launching another helper. Call
`initialize()` again before configuration checks, permission requests or starting
a replacement session. A version lookup alone does not initialize that session.

## Validation

After resolving overlays, the build loads the selected host and Windows test
dependency graphs with plain `go list -deps -test`, then runs `go mod verify`
before any tests or compilation. It avoids downloading unused module variants.
It runs helper protocol/lifecycle tests on the build host and cross-compiles the
selected Windows core. Windows CI also packages the Flutter example and enables
`SING_BOX_FLUTTER_WINTUN_SMOKE=1` for a real Wintun adapter lifecycle test. That test
requires elevation, starts a uniquely named adapter with `auto_route: false`,
closes the native core, and verifies adapter removal while preserving the runner's
default routes and DNS policy. Ordinary builds skip this opt-in test.
These checks do not establish external VPN connectivity. Before shipping,
test on an elevated Windows machine: adapter creation, DNS, IPv4/IPv6, outbound
route bypass, Wi-Fi/Ethernet changes, sleep/resume, start failure, explicit stop,
and app exit cleanup. Do not run real traffic tests in a job that depends on the
network routes being changed.
