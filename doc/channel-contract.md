# Native channel contract

This document is the implementation contract for the initial Android and iOS plugin.
All public documentation is in English. The package is `sing_box_flutter`, and the
repository is `idy/sing-box-flutter`. Native core inputs are pinned in
`native-core.json`. Build all Go code with the pinned **Tailscale Go** toolchain.

## Channels

- Method channel: `dev.idy.sing_box_flutter/methods`
- Event channel: `dev.idy.sing_box_flutter/events`

## Methods

- `initialize`: arguments `{appGroup?: String, providerBundleIdentifier?: String,
  notificationTitle: String}`. The two Apple identifiers are required on iOS.
  Initializes native directories and system integration; does not connect. Returns null.
- `getVersion`: returns the native core version. On iOS the app reads its bundled
  capabilities manifest; the extension verifies it against the linked engine on start.
- `checkConfig`: arguments `{config: String}` containing sing-box JSON. Android uses
  native engine validation. iOS performs app-side structural and compiled-capability
  preflight without linking a second Go engine; full engine validation occurs in the
  extension before startup. Returns null when those checks pass, otherwise a platform
  error. A successful iOS preflight does not guarantee startup. Never include config
  contents, auth keys, or other credentials in error messages.
- `requestPermission`: returns a boolean. Handles Android's activity result and
  the Apple VPN configuration consent. Cancellation returns false.
- `start`: arguments `{config: String, name: String}`. Starts the native VPN service.
  Returns null when the start request is accepted; connection state comes from events.
  Do not report connected until the native core and tunnel have started successfully.
- `stop`: stops the VPN and releases resources. Returns null.
- `getStatus`: returns a status event map.

Initialize must complete before all methods except `getVersion`. Methods must report
errors rather than silently simulating a connection. The VPN survives Flutter event
subscription cancellation and UI engine detachment. Native services must serialize
start/stop transitions and handle OS revocation and network changes.

## Events

Status: `{type: 'status', state: String, message?: String}`. Allowed states:
`disconnected`, `connecting`, `connected`, `disconnecting`, `error`.
A new subscriber receives the current status. The error message is bounded and must
not contain complete configs or credentials. The initial status is disconnected.

Log: `{type: 'log', level: String, message: String}`. Optional in the first release.
Do not forward unredacted upstream auth URLs, keys, or configuration dumps as logs.

## Linux process backend

Linux implements the same Dart platform API with `LinuxSingBoxFlutter`, registered
as a Dart plugin with native CMake bundling. It uses JSON-line requests/responses
on inherited pipes to a source-built `sing-box-core`; it opens no command socket.
Configurations never appear in command-line arguments. The bundled capabilities
fingerprint must match the helper handshake before initialization.

Linux uses the generated registry with no mobile `PlatformInterface`, preserving
upstream Linux TUN, routing and network monitors. `checkConfig` uses native engine
validation. `requestPermission` probes actual TUN creation and returns a boolean;
it does not elevate the process or display a consent dialog. Only the helper needs
TUN capabilities. Start/stop requests are serialized in the helper, and connected
is emitted only after the engine starts. The Linux start future remains pending
until startup succeeds or is cancelled; hosts must keep stop available while
connecting. A native close failure permanently marks that helper unusable with
`cleanup_failed`; the Dart backend retires it and requires initialization of a
replacement after process exit.

Subscriptions and widgets do not own the Linux connection. Flutter engine/app
exit closes stdin, which stops the helper and releases the TUN/routes. This differs
from the persistent mobile service lifecycle. Unexpected helper exit fails pending
requests, emits an error and requires initialization before reconnection. See
[Linux setup](linux-setup.md).

## Native core

With a host `sing_box_flutter.core` selection, use the small `native/mobile`
platform binding and generated constructor registry. Preserve the existing mobile
platform API while omitting the upstream RPC/profile-management layer. Tailscale is
optional. Artifacts and capabilities manifests are host-local under
`.dart_tool/sing_box_flutter`; native integration must reject stale or mismatched
artifacts, rather than fall back to a different core.

Hosts without a selection retain the standard upstream `experimental/libbox`
build/install path, using `android/libs/libbox.aar` and
`ios/Frameworks/Libbox.xcframework`. Binary artifacts are build outputs, not source
files. The iOS packet tunnel runs in its own extension process, outside Flutter;
only the PacketTunnel/Core subspec links Go. Shared storage, capability preflight,
and Flutter channels must not link the engine into the main app.
Android uses a foreground `VpnService`, protects outbound sockets and monitors the
underlying network. GPL source attribution and license notices must be retained.
