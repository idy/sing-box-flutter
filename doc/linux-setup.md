# Linux host setup

Linux uses a source-built sing-box helper, controlled by the Dart plugin over
inherited stdin/stdout pipes. The helper uses sing-box's native Linux TUN, routing
and network monitoring implementations. No Android VPN service, gomobile, system
sing-box installation, or Go runtime installation is needed on the target machine.

## Build and run

Use Flutter 3.47.2 or later, Python 3.11+, Git, CMake 3.19+, Clang, Ninja,
pkg-config and GTK 3 development headers. On Ubuntu:

```bash
sudo apt-get install clang cmake ninja-build pkg-config libgtk-3-dev libstdc++-12-dev
```

Add `sing_box_flutter.core` to the host pubspec as described in the
[README](../README.md#install-and-build-from-source). Linux requires a feature
selection; the historical mobile binary installer does not support Linux.

From the Flutter host application:

```bash
flutter pub get
```

```bash
dart run sing_box_flutter build --target linux --arch amd64
```

```bash
flutter build linux --release
```

Use `--arch arm64` for ARM64 Linux. The Go helper can be cross-compiled from
macOS or Linux; the Flutter Linux application itself must be built on Linux with
its matching architecture toolchain. Without `--arch`, the core builder uses the
build host's architecture. Changing features or architectures requires rebuilding
the core before building the Flutter application.

The plugin's CMake integration validates the selected core and installs these
files beside the Flutter application:

```text
bundle/
  your_app
  lib/
    sing_box_flutter/
      sing-box-core
      core-capabilities.json
```

Distribute the complete Flutter bundle. The helper and capabilities manifest must
stay together. A missing, stale or mismatched core is an error; the plugin does
not fall back to another executable from PATH.

## TUN permissions

Userspace packet processing still needs a kernel TUN interface. Linux requires
`/dev/net/tun` and `CAP_NET_ADMIN` to create the interface and configure its routes.
`CAP_NET_RAW` also enables raw sockets used by applicable sing-box features. Grant
capabilities to the **bundled helper**, leaving the Flutter application as a normal
user process. For an x64 release example, from the host application:

```bash
sudo setcap cap_net_admin,cap_net_raw+ep build/linux/x64/release/bundle/lib/sing_box_flutter/sing-box-core
```

```bash
build/linux/x64/release/bundle/sing_box_flutter_example
```

Replace the executable name with your application's name. ARM64 builds use
`build/linux/arm64/` instead of `build/linux/x64/`. Reapply capabilities after a
rebuild, copy or installation that replaces the helper. Installations should use
a root-owned helper and restrict execution to the intended users. The destination
filesystem must support file capabilities; `nosuid`, container capability bounds
and sandbox packaging can prevent them from taking effect.

`requestPermission()` does not open an elevation dialog. It checks the helper's
actual permissions by creating and immediately closing an unnamed TUN, without
assigning addresses or routes. It returns false if the device is unavailable or
the helper cannot create a TUN. It does not run sudo, pkexec, or change system
permissions. Do not run the GTK application itself as root.

In a container, expose `/dev/net/tun` and grant `NET_ADMIN` (and `NET_RAW` when
needed) to that container. A container test changes only its own network namespace.
Snap/Flatpak need their own explicit networking/device integration; no sandbox
package is supplied by this plugin.

## Configuration and lifecycle

Use the existing Dart API: `initialize`, `checkConfig`, `requestPermission`,
`start`, `stop`, status events and `getVersion`. Apple identifiers and the Android
notification title are unused on Linux. Configuration is passed through pipes,
not command-line arguments or a temporary config file. Raw engine logs are not
forwarded. `checkConfig` performs native engine validation without starting TUNs.
Each IPC request must fit within 16 MiB including its JSON envelope, UTF-8 encoding
and trailing newline. A larger request fails locally with a sanitized
`PlatformException` (`request_too_large`) and leaves an active connection running.

State is stored under `$XDG_DATA_HOME/<application-id-hash>/sing_box_flutter` when
`XDG_DATA_HOME` is an absolute path. An unset, empty or relative value uses
`$HOME/.local/share/<application-id-hash>/sing_box_flutter`. The hash is the full
SHA-256 digest of the host's GTK `GApplication` ID, normally the `APPLICATION_ID`
in the host's `linux/CMakeLists.txt`. Keep this ID stable across releases and use a
different ID for each application. State survives versioned installation paths,
launcher symlink updates and executable moves; applications with the same executable
basename remain separate when their application IDs differ. Old basename-only and
executable-path-hash state directories are not reused.

Custom hosts can supply a stable ID when building Flutter, for example
`flutter build linux --dart-define=SING_BOX_FLUTTER_APPLICATION_ID=dev.example.Vpn`.
This compile-time override takes precedence over the GTK ID. IDs must have at least
two nonempty dot-separated ASCII elements containing letters, digits, `_` or `-`,
with no element starting with a digit, and must be at most 255 characters. A missing
or invalid identity fails with a sanitized `application_identity` error; the backend
does not fall back to the executable name or a version-dependent path.

If `HOME` is also unset, empty or relative, the backend calls POSIX `mkdtemp` to
atomically create a fresh directory with mode `0700` in the system temporary
directory. Before creation it canonicalizes the parent and verifies ownership and
permissions throughout its ancestry. Root-owned sticky directories such as `/tmp`
are allowed; nonsticky group/other-writable ancestors and foreign-owned ancestors
are rejected before any directory is created. A safe symlinked temporary parent is
supported. The newly created child is passed to the helper without resolving it,
so replacing that child with a symlink cannot hide the replacement from the helper.
The directory is allocated lazily when launching the helper. The helper tracks
its identity and removes its contents and the directory after parent EOF or
SIGINT/SIGTERM, once native shutdown and all startup workers finish. Symlink
targets outside the directory are preserved. If launching the helper fails, the
backend removes the unused empty directory. A replacement helper gets fresh
fallback state after the old process exits; normal stop/start on the same helper
reuses its state. Forced termination such as SIGKILL can prevent cleanup.
This fallback does not provide persistence across helper replacements or
application launches. Set a valid `HOME` or
`XDG_DATA_HOME` when persistent caches or other state are required. State paths
never resolve relative to the application's working directory, and environment
variables such as `USER` and `UID` do not establish ownership.

Before opening state, the helper creates missing directories with mode `0700` and
checks ownership against its real process UID. Existing state and `tmp` directories
must belong to that UID and have no group or other permissions. Symlinks at those
private entries, foreign owners and unsafe permissions are rejected; the helper
never repairs them with `chmod`. Ancestors may be ordinary user-owned or root-owned
directories, but must not be writable by other users, except for root-owned sticky
directories such as `/tmp`. Trusted ancestor symlinks, including `/home` pointing
to `/var/home`, are resolved through pinned descriptors and the target ancestry is
validated. The helper uses the resulting canonical path; the private state and
`tmp` entries themselves must be actual directories.

A minimal direct-routing configuration is:

```json
{
  "dns": {
    "servers": [{"type": "udp", "tag": "dns-direct", "server": "1.1.1.1"}],
    "final": "dns-direct"
  },
  "inbounds": [{
    "type": "tun",
    "tag": "tun-in",
    "address": ["172.19.0.1/30", "fdfe:dcba:9876::1/126"],
    "auto_route": true,
    "stack": "system"
  }],
  "outbounds": [{"type": "direct", "tag": "direct"}],
  "route": {"auto_detect_interface": true, "final": "direct"}
}
```

This sends captured traffic directly. Replace the routing and outbounds/endpoints
for a real VPN. When the core omits the `local` DNS capability, specify DNS servers
explicitly; an omitted DNS block would request the engine's local DNS fallback. `stack: "system"` uses sing-box's system network stack;
`stack: "gvisor"` uses its userspace stack when compiled. Both require TUN
permissions. Keep `auto_detect_interface` or an appropriate outbound interface
binding to avoid routing the core's own traffic back into its TUN.

The plugin emits `connected` only after sing-box starts successfully. Cancelling
a status subscription or disposing a widget does not stop the core. Unlike the
mobile VPN services, the Linux helper belongs to the Flutter engine: engine/app
exit closes its control pipe, then the helper closes the core, TUN and routes.
The helper also cleans up on SIGINT/SIGTERM. A core process crash is reported as an
error and requires initialization again before reconnecting. A failed native
cleanup also retires the helper: later start/stop requests cannot report a clean
disconnect or reuse that process. The backend waits for it to exit before a new
initialization can launch a replacement. The example keeps Disconnect available
while connecting, so a pending startup can be cancelled. Initialization
refreshes and broadcasts the helper's current status; initializing an already
connected helper preserves its connection. This integration is not a persistent
system daemon or a systemd service.

All plugin objects in one Flutter engine use the same registered backend. Separate
application instances are separate helpers; avoid running conflicting TUN/routing
configurations concurrently.

Linux supports the selected protocol catalogue with the platform restrictions
reported by the builder. Naive outbound is unavailable in the static helper,
and bridge, Tor and USB/IP are not integrated. Transparent redirect/TProxy and
resolved services require their own Linux routing/firewall/service configuration;
the plugin does not install firewall rules or configure systemd-resolved for you.

Before distributing an application, verify forwarding, DNS, IPv4/IPv6,
sleep/wake, interface changes, explicit stop and application exit on the intended
Linux distributions.
