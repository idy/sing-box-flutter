# WGC endpoint

The native build registers `wgc` in libbox. Handshakes, encryption, replay
protection and peer sessions come directly from `github.com/idy/wgc`; the small
`native/wgc` adapter connects that library to sing-box's existing userspace stack.
Flutter continues to use `checkConfig` and `start` with ordinary sing-box JSON.
There is no separate SDK configuration model, VPN service, or Flutter API.

## Configuration

Each endpoint selects one peer. Use normal sing-box routing or selectors for
multiple WGC endpoints. Replace the key placeholders with canonical Base64 keys
and configure the server to allow the inner address `10.7.0.2`:

```json
{
  "log": {"disabled": true},
  "inbounds": [{
    "type": "tun", "tag": "tun-in", "address": ["172.19.0.1/30"],
    "stack": "gvisor", "auto_route": true, "dns_mode": "hijack",
    "route_exclude_address": ["203.0.113.10/32"]
  }],
  "endpoints": [{
    "type": "wgc", "tag": "wgc",
    "address": ["10.7.0.2/32"], "mtu": 1280,
    "private_key": "<local-private-key>",
    "peer_public_key": "<remote-public-key>",
    "peer_endpoint": "203.0.113.10:51820"
  }],
  "dns": {
    "servers": [{"type": "udp", "tag": "remote", "server": "1.1.1.1", "detour": "wgc"}],
    "final": "remote"
  },
  "route": {
    "auto_detect_interface": true, "default_domain_resolver": "remote",
    "rules": [{"port": 53, "action": "hijack-dns"}], "final": "wgc"
  }
}
```

`pre_shared_key` is optional. TCP and UDP use the existing sing-box stack and
resolver. UDP is the default outer transport. Interface updates reopen the outer
transport while preserving the WGC connection and userspace stack. The host's existing
Android/iOS VPN setup, storage, lifecycle, consent, and status API are unchanged. Install the
matching WGC-enabled native artifacts; an unmodified upstream libbox does not
register this endpoint.

## Compiled custom transports

An application Go package can call `wgc.RegisterTransport(name, factory)` from
`github.com/idy/sing-box-flutter/native/wgc`. A factory receives the native context,
peer endpoint and `transport_options` JSON and returns the existing
`github.com/idy/wgc.Transport` interface. Use `wgc.NewUDPTransport` for a protected
base socket when building wrappers. Factories must honor cancellation and unblock
I/O on Close. Select them with `"transport": "your-name"` on the endpoint.

Compile the registration package through a build manifest:

```json
{"extensions": [{"package": "example.com/team/transport/register", "module": "example.com/team/transport", "version": "v1.2.3"}]}
```

```bash
dart run sing_box_flutter build --target apple --wgc-config wgc.core.json
```

A local extension may specify `path`, relative to the manifest. Export private
module settings such as `GOPRIVATE`; Git/SSH credentials remain in the normal
user environment, not the manifest. `GONOPROXY`, `GONOSUMDB`, and explicit
`GOPROXY` settings are retained under the pinned Go toolchain. Build support and
local encrypted round trips do not establish signed-device VPN acceptance.
