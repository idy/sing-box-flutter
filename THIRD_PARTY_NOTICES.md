# Third-party notices

`sing_box_flutter` is an independent integration, not affiliated with or endorsed
by SagerNet or Tailscale.

## sing-box and libbox

- Source: https://github.com/SagerNet/sing-box
- Version: 1.14.4, revision `c005f35300ce76427dc285481ec5695f5b179cd2`.
- Copyright (C) 2022 by nekohasekai <contact-sagernet@sekai.icu>.
- License: GPL-3.0-or-later, included in `LICENSE`.

The upstream README additionally states:

> In addition, no derivative work may use the name or imply association with this
> application without prior consent.

Native artifacts are built separately. The selectable core adapts the pinned
libbox platform helpers under `native/mobile`; copied files retain their upstream
copyright and the exact source pin is recorded in `native/mobile/NOTICE`. It replaces
the upstream mobile management layer and generates only selected registrations.
Distributors must retain applicable notices and provide corresponding source as
required by the licenses.

## WGC

- Source: https://github.com/idy/wgc
- Version: `v0.2.1-0.20261107180014-c3ca5bb852e4`.
- License: MIT; collected from the pinned module with native distributions.

The `native/wgc` binding uses this dependency directly. Its protocol source is
not vendored here. The binding follows this project's GPL-3.0-or-later license.

## Wintun

- Source and official binaries: https://www.wintun.net/
- Version: 0.14.1; official archive, architecture-specific driver, and license
  hashes are pinned in `tool/wintun.json`.
- Runtime source: `github.com/sagernet/sing-tun` at
  `v0.9.7-0.20261023115432-040db71fcd33`, module checksum
  `h1:0+op+qbxh/M/JsqIG8tkEwPM+5pZtBeicf1ciMxp4OI=`. Its
  `internal/wintun/{amd64,arm64}/wintun.dll` resources match the official hashes
  and are embedded in the helper, then loaded through sing-tun's in-memory loader.
- Copyright (C) 2018-2021 WireGuard LLC. All Rights Reserved.
- The embedded official DLL uses the exact license from its release archive,
  retained in this package as `native/windows/LICENSE.wintun` and in Windows apps
  as `sing_box_flutter/Legal/LICENSE.wintun`. Preserve it when redistributing an
  application. Wintun source is GPL-2.0. No separate driver DLL is bundled.

## Platform references and toolchain

Native platform code references the official GPL-3.0-or-later clients:
https://github.com/SagerNet/sing-box-for-android and
https://github.com/SagerNet/sing-box-for-apple. Adapted files retain their notices.

The Tailscale Go toolchain is pinned to
`63ae404c8203317fd3c82d972e5dc8f0fcb425cb` from https://github.com/tailscale/go
(BSD-3-Clause). Native Go module manifests identify the remaining dependencies;
distributors must include the notices required by their selected build profile.
