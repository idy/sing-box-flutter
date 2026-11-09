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
    "type": "tun", "tag": "tun-in", "address": ["172.19.0.1/30", "fd00:7::1/126"],
    "stack": "gvisor", "auto_route": true, "dns_mode": "hijack"
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
    "rules": [{"port": 53, "action": "hijack-dns"}, {"ip_version": 6, "action": "reject"}],
    "final": "wgc"
  }
}
```

The TUN captures both IPv4 and IPv6. This IPv4-only WGC peer example rejects
IPv6 after DNS handling; add a server-authorized IPv6 address to the WGC endpoint
before removing that reject rule. Keep `auto_detect_interface` enabled: the
outer socket uses Android protection or Apple's physical-interface binding.
Application traffic to the peer's public IP still follows the normal VPN rules.

`pre_shared_key` is optional. TCP and UDP use the existing sing-box stack and
resolver. UDP is the default outer transport. Interface updates reopen the outer
transport while preserving the WGC connection and userspace stack. Fatal packet-loop
failures instead rebuild that connection and stack with cancellable one-second
retries, resetting in-flight flows; stopping the endpoint ends recovery. The host's existing
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
{"extensions": [{
  "package": "example.com/team/transport/register",
  "module": "example.com/team/transport",
  "version": "v1.2.3",
  "sum": "h1:<trusted-module-checksum>",
  "go_mod_sum": "h1:<trusted-go.mod-checksum>"
}]}
```

Remote entries require both checksum pins. Obtain `Sum` and `GoModSum` from
`go mod download -json` using a trusted module source, then review and commit them
as `sum` and `go_mod_sum` in the manifest. The build enforces these values even
when `GOPRIVATE` or `GONOSUMDB` disables the public checksum database. Commit the
module's complete `go.sum` as well: its authenticated dependency checksums are
promoted into the main build. Any resolved checksum missing from the trusted
upstream, adapter or extension locks is rejected before compilation, including
builds with no custom extensions. Extension module paths must not collide with
the existing core dependency graph, so a local replacement cannot override it.

```bash
dart run sing_box_flutter build --target apple --wgc-config wgc.core.json
```

A local extension may specify `path`, relative to the manifest, and omit `version`.
The default matches the module's major version, such as `v2.0.0` for a `/v2` module.
The loader computes `source_sha256` for local entries; omit `sum` and `go_mod_sum`.
The build copies local modules into private snapshots and verifies each copy;
edits to the original directory after capture apply to the next build. Reserved
`.build` and `.wgc-inputs` generated state is excluded from both fingerprinting
and copying, including when a module contains this plugin checkout. Replacements
use stable relative paths so build information does not expose checkout paths.
Export private module settings such as `GOPRIVATE`; Git/SSH credentials remain in the normal
user environment, not the manifest. `GONOPROXY`, `GONOSUMDB`, and explicit
`GOPROXY` settings are retained under the pinned Go toolchain. Build support and
local encrypted round trips do not establish signed-device VPN acceptance.
