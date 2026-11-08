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
- `getVersion`: returns the embedded libbox version string.
- `checkConfig`: arguments `{config: String}` containing sing-box JSON. Returns null
  if valid; otherwise throws a platform error. Never include configuration contents,
  auth keys, or other credentials in error messages.
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

## Native core

Use the pinned upstream `experimental/libbox` API. Tailscale must be included.
Android uses `android/libs/libbox.aar`; iOS uses `ios/Frameworks/Libbox.xcframework`.
The binary artifacts are build outputs, not source files. Document any extra host
setup clearly. The iOS packet tunnel runs in its own extension process, outside
Flutter; expose its provider as a separate source/subspec for host extension targets.
Android uses a foreground `VpnService`, protects outbound sockets and monitors the
underlying network. GPL source attribution and license notices must be retained.
