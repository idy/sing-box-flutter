# Windows and Wintun

The Windows backend runs a host-selected sing-box core in a bundled helper
process. sing-box creates the native TUN interface with the official Wintun DLL;
Flutter uses the same `SingBoxFlutter` API as Android and iOS. Windows 10 or newer
is required. Core builds support x64 (`amd64`) and ARM64 (`arm64`); the Flutter
runner and the core must have matching architectures.

## Build the selected core

Install Flutter 3.47.2+, Python 3.11+, Git, and Visual Studio's **Desktop development
with C++** workload, including the Windows SDK and CMake 3.27+ (update the Visual
Studio CMake component if needed). Packaging reads CMake's structured installer
records so complete destination paths, including semicolons, remain intact. Flutter's Windows packaging runs
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
command cannot provide a Windows core. With a host core selection, `--target all`
builds Android, Apple and macOS; legacy standard-core builds still select Android
and Apple only. Build Windows explicitly in either case.

Artifacts stay in the host's `.dart_tool/sing_box_flutter/`, with a receipt bound
to the host pubspec, selected features, native source, compiler pins, Wintun hashes,
and architecture. The Windows pointer is `windows.json`; mobile cores retain their
separate `native.json`, so building one platform does not replace the other's
active artifact. Windows artifacts use
`builds/windows/<combined-sha256>/WindowsCore`, where the full digest binds the
recipe fingerprint and all artifact file hashes. Full recipe and file identities
remain in the receipt and build metadata. CMake validates the path and digest in
one read-only invocation, then rechecks the configured pair before every build.
Installation verifies the configured receipt, path and digest before copying the
helper, including when invoked directly through `cmake --install`. On Windows,
read-only handles pin source files, directory ancestors, the receipt and host
pubspec in the invoking CMake process until it exits. These handles deny write
and delete sharing while CMake copies the inputs. The installed helper bytes and
current selection are also checked after copying; a mismatched helper is removed
and installation fails. Non-Windows packaging checks use the same final byte
validation. Every install invocation has an atomically allocated record handoff,
so concurrent prefixes or components do not share mutable record files. Components
can stage the host before a separately owned helper; Runtime resources follow the
host, and each helper installation is verified in its own component. Whole-bundle
installation requires the helper beside the host.
It bundles `sing_box_flutter_core.exe` beside the host executable and installs
capabilities, build provenance, and notices under `sing_box_flutter/`. The exact
official driver license is package-owned at `native/windows/LICENSE.wintun` and
retained under `sing_box_flutter/Legal/LICENSE.wintun` in the app.
Keep the complete Flutter release directory together when distributing the app.
Rebuild the core after changing its feature selection or updating the plugin.
A normal Flutter build reports a missing/stale core instead of downloading one.

## Secure helper deployment

Deploy the **complete host application** through a trusted elevated installer to a
new administrator-managed directory, normally `%ProgramFiles%\\YourVendor\\YourApp`.
The installer must obtain its files from an authenticated release package, create
its directories and files with their final restrictive permissions, and assign
ownership to **Administrators**, **SYSTEM**, or **TrustedInstaller**. Grant ordinary
users read/execute access only. Use the same policy for updates; stop the app
before replacing its installed files. Do not move a user-owned build directory
into Program Files, reuse an untrusted existing directory, or rely on a build-time
hash to make a writable deployment safe. The host, its Flutter runtime/plugin,
and its assets must already be trusted when the elevated host starts.

Before every helper launch, the native plugin derives the helper location from
the running host and opens the volume root, every directory, and
`sing_box_flutter_core.exe` separately with no-follow semantics. It requires a
local fixed volume with persistent ACLs and rejects reparse points, a multi-link
helper, and owners outside the three trusted identities. It rejects
DACL grants allowing any other identity to modify data, attributes, extended
attributes, ownership or permissions, or delete/replace the object or its child.
Inherited grants and generic access masks are checked too; unsupported ACL grant
types fail closed. Ancestors may permit creating unrelated sibling entries, as
normal Windows volume roots do, but the application directory cannot permit
ordinary users to create children. An individual administrator's user SID is
**not** trusted ownership: the same user at medium integrity can otherwise obtain
implicit permission to change the DACL.

The plugin retains read-only handles denying write/delete sharing on the entire
chain until Dart's `Process.start` completes. Existing incompatible write/delete
handles cause validation to fail. Native code obtains the pinned volume's NT
device name with `GetFinalPathNameByHandleW(VOLUME_NAME_NT)` and returns a Win32
device image path such as
`\\.\Global\GLOBALROOT\Device\HarddiskVolume3\Program Files\VPN\sing_box_flutter_core.exe`.
It verifies and retains the `Global` link in the effective device directory and the
global `GLOBALROOT` link, requiring trusted owners, read/query-only untrusted
grants, and their expected targets. A global-fallback `Global` link is rejected
when a new local alias could shadow it. Launch therefore does not resolve the
original DOS drive letter or an untrusted local `GLOBALROOT` alias. The source
volume is also checked against the already-running host's native image path,
rejecting a drive remap that happened before acquisition.

The [Windows namespace documentation](https://learn.microsoft.com/en-us/windows/win32/fileio/naming-a-file#nt-namespaces)
explains `GLOBALROOT` and the distinction between Win32 device and extended file
paths. The extended `\\?\Volume{GUID}\...` image form fails `CreateProcessW` with
error 87 and is not used. Native code enforces
[CreateProcessW's documented `MAX_PATH` image-name limit](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-createprocessw)
because [Dart's Windows process launcher](https://github.com/dart-lang/sdk/blob/main/runtime/bin/process_win.cc)
passes a quoted command line with a null `lpApplicationName`; the helper directory
is passed explicitly as the working directory. Configuration is sent through the
pipe only after process creation and successful native lease release. Missing
native validation, unsafe deployment or uncertain lease handling fails closed with
a fixed `core_untrusted` error, without starting an unverified helper or sending it
VPN configuration. The plugin does not repair permissions, alter token privileges,
or provide an unsafe development/CI override.

Ordinary checkout, Downloads, temporary and user-owned Flutter build directories
are therefore unsuitable for running this elevated backend. Build normally, then
install an authenticated build using the policy above and launch the **installed**
host. Development builds require the same secure deployment as release builds;
`flutter run` from a writable checkout is not a supported elevated launch path.

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
embedded elevation level. After secure deployment, launch its installed
executable to accept the Windows UAC prompt. A plugin cannot change the host's
manifest or make a user-writable build directory safe.

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

An explicit `experimental.cache_file.path` outside application storage must also
have a verified local hierarchy without reparse points. Its immediate parent and
existing file must already be private at the required integrity, or be created
atomically with that policy. Unsafe writable external paths are rejected without
repair. First admission uses an exclusive-sharing metadata probe to reject
pre-existing read/write/delete handles, then overlaps that probe with the retained
file pin. Concurrent checks can reuse the current core's admitted file. The cache
identity remains pinned through all core/check cleanup; checking a configuration
with the cache enabled can create an empty private file. Do not harden untrusted
external caches in place; historical metadata rights are not revoked by ACL changes.
Deleting and recreating a corrupt cache is blocked while its file is leased: stop
and exit the helper before manually cleaning it up.

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
selected Windows core. Windows CI compiles and runs standalone native launch-policy tests without a
Flutter framework build. These cover trusted and untrusted owners, mutation ACLs,
no-follow path validation, retained write/delete handles, directory replacement,
hard links, reparse points, DOS aliases, and actual launches through the pinned
global device path after a DOS drive remap. Both explicit image names and Dart's
null-application-name/explicit-working-directory call shape are exercised, and the
mapped child's file identity is checked before it runs. These are Windows runtime
checks; passing Dart tests on another platform does not establish compatibility.
The tests require elevation and fail if their secure fixtures
cannot be created; they do not bypass the production policy for CI.
Windows CI also packages the Flutter example and enables
`SING_BOX_FLUTTER_WINTUN_SMOKE=1` for a real Wintun adapter lifecycle test. That test
requires elevation, starts a uniquely named adapter with `auto_route: false`,
closes the native core, and verifies adapter removal while preserving the runner's
default routes and DNS policy. Ordinary builds skip this opt-in test.
These checks do not establish external VPN connectivity. Before shipping,
test on an elevated Windows machine: adapter creation, DNS, IPv4/IPv6, outbound
route bypass, Wi-Fi/Ethernet changes, sleep/resume, start failure, explicit stop,
and app exit cleanup. Do not run real traffic tests in a job that depends on the
network routes being changed.
