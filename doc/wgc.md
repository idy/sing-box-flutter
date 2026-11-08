# WGC in sing_box_flutter

`sing_box_flutter` 0.2.0 integrates WGC into the same native libbox VPN as other
sing-box protocols. A WGC endpoint owns its WGC session inside that runtime.
Flutter selects its peer and compiled transport; packet processing stays in Go.
There is one VPN service and one TUN, including when WGC and Tailscale endpoints
appear in the same configuration.

The supported minimum versions are Android 7 / API 24, iOS 15, and macOS 12.
Native artifacts must include the WGC endpoint and any selected custom transport.
Adding a Dart dependency alone does not compile or install them. Apple apps also
need an embedded, signed Packet Tunnel extension and matching entitlements.

## Migrate from wireguard_conn_flutter

Replace the old dependency and import:

```yaml
dependencies:
  sing_box_flutter: ^0.2.0
```

```dart
import 'package:sing_box_flutter/sing_box_flutter.dart';
```

The version 1 profile names remain `WgcConfiguration`, `WgcPeer`, `WgcTransport`,
and `WgcNetworkSettings`. `validateWireGuardKey` validates canonical Base64 keys
with exactly 32 decoded bytes. Both private and public keys use this encoding;
encoding validation does not establish ownership, peer trust, or authorization.

Replace `WgcNe` / `WgcNe.android` with `SingBoxFlutter`, `start` with `startWgc`,
and the old status API with `status` / `getStatus`. Use `stop` for explicit
disconnection. Subscriptions do not own the native service's lifetime. The
`VpnStatus` / `VpnConnectionState` types replace the old WGC-specific status
model; old native handles and counters are not part of this migration API.

Initialize Apple integration with the host's actual identifiers:

```dart
final vpn = SingBoxFlutter();
await vpn.initialize(
  appGroup: 'group.com.example.app',
  providerBundleIdentifier: 'com.example.app.PacketTunnel',
  keychainAccessGroup: 'ABCDE12345.com.example.app.shared',
  notificationTitle: 'Example VPN',
);
```

`appGroup` must be present in both the host and Packet Tunnel extension's App
Group entitlements. The optional `keychainAccessGroup` must be the fully expanded
shared Keychain access group in both targets' entitlements. For example, an
entitlement of `$(AppIdentifierPrefix)com.example.app.shared` may expand to
`ABCDE12345.com.example.app.shared`; pass the expanded string, not the build
variable. If the argument is omitted, the resolved app default Keychain group
must also be accessible to the extension. App Groups and Keychain access groups
are separate entitlements; the App Group does not itself grant Keychain access.
The native Apple provider stores configuration in device-only Keychain items.
See [Apple host setup](ios-setup.md) for extension provisioning.

On Android, omit Apple identifiers:

```dart
final vpn = SingBoxFlutter();
await vpn.initialize(notificationTitle: 'Example VPN');
```

Load the old version 1 JSON from your secure configuration source, validate it,
and start it through the new controller:

```dart
final configuration = WgcConfiguration.fromJsonString(profileJsonFromSecureSource);
final subscription = vpn.status.listen((status) {
  // Update the UI from status.state. Do not log profile JSON or keys.
});

await vpn.checkWgcConfig(configuration);
if (await vpn.requestPermission()) {
  await vpn.startWgc(configuration: configuration, name: 'My WGC VPN');
}

// Call these when the user explicitly disconnects / the UI unsubscribes.
await vpn.stop();
await subscription.cancel();
```

`checkWgcConfig` converts the profile and asks the native core to validate the
result. `startWgc` sends the same complete sing-box JSON used by `start`.
Both accept an optional `peer` naming a declared peer. `startWgc` acknowledges
startup; observe native status for the connection outcome. A connected local
service does not prove remote peer reachability or working internet access.

## Typed profile and generated configuration

A minimal programmatic profile is:

```dart
final configuration = WgcConfiguration(
  privateKey: privateKeyFromSecureSource,
  addresses: ['10.7.0.2/32'],
  peers: [
    WgcPeer(
      id: 'exit',
      publicKey: serverPublicKey,
      endpoint: '203.0.113.10:51820',
    ),
  ],
);

final legacyV1Json = configuration.toJsonString();
final singBoxJson = configuration.toSingBoxConfigString(peer: 'exit');
await vpn.checkConfig(singBoxJson);
```

`toJson` / `toJsonString` preserve the legacy WGC version 1 schema. They do not
produce a complete sing-box configuration. `toSingBoxConfig` returns a map;
`toSingBoxConfigString` encodes that map. Both JSON forms contain private keys
and transport options. Treat them as secrets; `toString` and validation errors
omit their contents. The legacy profile limit remains 128 KiB of UTF-8 JSON.

The generated sing-box 1.14.4 configuration has:

- One TUN inbound with the profile's addresses, MTU (1280–1420), and gVisor stack.
- One endpoint tagged `wgc`, carrying the version 1 profile, the selected peer,
  and the resolver tag `dns-remote`.
- UDP DNS `dns-remote` and TCP DNS `dns-remote-tcp`, both using the numeric
  `dns_server` host and port with `detour: wgc`. A TCP DNS rule selects the TCP
  server; the default selects UDP. DNS query strategy follows tunnel families.
- TUN `dns_mode: hijack` and `dns_address` from `network.dns_servers`, plus route
  rules hijacking port 53 and sniffed DNS traffic. `dns_server` is the upstream;
  `network.dns_servers` are the DNS addresses presented to the operating system.
- `route_address` from `network.included_routes` and `route_exclude_address`
  containing configured exclusions plus a host route for **every** declared
  outer peer. Numeric IPv4 peers use `/32`; numeric IPv6 peers use `/128`.
- A default route selecting `wgc`, automatic interface detection, and disabled
  configuration-generated logging.

Omitted or null includes select default routes for the tunnel families:
`0.0.0.0/0`, `::/0`, or both. An explicit empty includes list means **no included
routes**. It stays empty, and the converter disables TUN `auto_route` so upstream
sing-box cannot interpret an empty `route_address` as default route capture.
Split includes are preserved, and each included route requires a corresponding
tunnel address family. Exclusions may contain either address family.

Defaults match the version 1 profile: the first peer, UDP transport, and MTU 1280.
A profile with IPv4 uses upstream `1.1.1.1:53` and tunnel DNS `198.18.0.2`.
IPv6-only profiles use `[2606:4700:4700::1111]:53` and `2001:2::2`.
Peer endpoints and DNS endpoints require numeric, unzoned hosts and a port from
1 to 65535; IPv6 endpoint hosts require brackets. There are at most eight tunnel
addresses, 32 peers, eight tunnel DNS addresses, and 1,024 entries in each input
route list. Transport chains permit eight explicit wrapper layers. Native
validation additionally checks transport registration and its option schemas.

## Legacy sing_box overrides require manual migration

An old version 1 `sing_box` object replaced the old plugin's generated routing
configuration. The model preserves this object for inspection and round trips,
but **every non-null override, including `{}`, is rejected by
`toSingBoxConfig`, `toSingBoxConfigString`, `checkWgcConfig`, and `startWgc`**.
The converter never silently discards custom routing or DNS policy.

Move that policy into a complete outer sing-box configuration before removing the
legacy override. Its TUN, DNS, and route rules now belong at the top level. Replace
old WGC outbounds with a WGC endpoint and put the remaining version 1 profile in
its `configuration` field. Reference its tag from DNS detours and routing rules.
The endpoint contract is:

```json
{
  "endpoints": [{
    "type": "wgc",
    "tag": "wgc",
    "configuration": {
      "version": 1,
      "private_key": "<canonical Base64 private key>",
      "addresses": ["10.7.0.2/32"],
      "peers": [{
        "id": "exit",
        "public_key": "<canonical Base64 server public key>",
        "endpoint": "203.0.113.10:51820"
      }]
    },
    "peer": "exit",
    "resolver": "dns-remote"
  }]
}
```

Replace the key placeholders with securely supplied values. This is an endpoint
fragment; the containing configuration must define the TUN, resolver and routes.
`peer` and `resolver` are optional at the endpoint level; `peer` must identify a
declared peer when supplied. Validate your complete migrated JSON with
`checkConfig`, then start it with `start(config: json, name: 'My VPN')`.

Ordinary plugin configuration remains sing-box JSON. To route selected traffic
through WGC and other traffic through Tailscale or other supported protocols,
compose their endpoints and route rules in that single configuration. Do not
create a second TUN or start a second VPN controller for WGC. Preserve outer
socket exclusions and the intended DNS policy when changing the generated map.
The WGC endpoint forwards TCP and UDP; raw ICMP forwarding is not promised.

## Compile custom transports into libbox

Custom transports execute inside Go. Register factories or wrappers with
`github.com/idy/sing-box-flutter/core/transport`; replace imports of the old
`github.com/idy/wgc-ne/core/transport` package. The WGC interface is from
`github.com/idy/wgc` (Go package name `wireguard`). Keep datagram boundaries and
remote peer identity, honor cancellation, and close owned resources. On Android,
protect every outer socket using the transport package's context-aware socket
helpers, including reconnects and auxiliary sockets.

Replace the old `wgc_ne.core.yaml` build input with `wgc.core.json`:

```json
{
  "extensions": [{
    "package": "example.com/team/transport/register",
    "module": "example.com/team/transport",
    "version": "v1.2.3"
  }]
}
```

An entry can also supply `"path": "../transport"` for a local module; paths are
relative to the manifest. Keep the package, module, and version fields. Private
Go module authentication belongs in the normal Go/Git environment, not this file.

```bash
dart run sing_box_flutter build --target apple --wgc-config wgc.core.json
```

```bash
dart run sing_box_flutter build --target android --wgc-config wgc.core.json
```

The source-checkout equivalent is
`python3 tool/build_libbox.py --target apple --wgc-config wgc.core.json` (or
`--target android`). Link/install the resulting libbox artifacts in the host and,
on Apple, its extension. These replace separate WGC NE cores. See
[native builds](native-build.md) for toolchain and artifact requirements.

Dart still selects the compiled transport by name and JSON options:

```dart
final transport = WgcTransport(
  type: 'my-wrapper',
  options: {'mode': 'example'},
  base: WgcTransport(type: 'udp', options: {'listen': ':0'}),
);
```

A wrapper's omitted base selects UDP according to native registry rules. Unknown
registrations fail instead of falling back. Dart preserves custom names and
options; registration and factory/wrapper constraints are native checks. No
per-packet Dart callback or separate WGC Flutter plugin is involved. Applicable
WGC, sing-box, and combined-binary license obligations still apply.

## Validation boundary

Dart tests cover profile validation, immutable options, legacy round trips, and
the generated sing-box structure. Native configuration checks are separate from
VPN traffic tests. This guide makes no signed-device validation claim. Test the
embedded extension/service, IPv4/IPv6 routes, DNS over UDP and TCP, peer exclusions,
permission changes, and network transitions on your supported physical devices
before shipping.
