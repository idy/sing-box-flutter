# Third-party notices

`sing_box_flutter` is an independent Flutter integration. It is not affiliated
with or endorsed by SagerNet or Tailscale.

## sing-box and libbox

- Source: https://github.com/SagerNet/sing-box
- Pinned version: 1.14.4
- Pinned revision: c005f35300ce76427dc285481ec5695f5b179cd2
- Copyright (C) 2022 by nekohasekai <contact-sagernet@sekai.icu>
- License: GPL-3.0-or-later; the license text is included in `LICENSE`.

The upstream README additionally states:

> In addition, no derivative work may use the name or imply association with this
> application without prior consent.

Native libbox artifacts are built separately. Applications distributing them must
retain applicable notices and provide corresponding source as required by the
licenses. This package does not claim to be an official sing-box application.

## WGC

- Source: https://github.com/idy/wgc
- Pinned version: `v0.2.1-0.20261107180014-c3ca5bb852e4`
- License: MIT. Its original license is collected from the pinned Go module for
  the native distribution; dependency license copies are not stored here.

The WGC session, configuration, transport registry, and Apple Keychain components
were migrated from https://github.com/idy/wgc-ne, GPL-3.0-or-later. They are
maintained here without depending on the retired repository. WGC's MIT license
does not change the license of the combined sing-box native core.

## Official platform clients

The native integrations use these official platform clients as implementation
references. Adapted files retain their original notices:

- https://github.com/SagerNet/sing-box-for-android
- https://github.com/SagerNet/sing-box-for-apple
- Copyright (C) 2022 by nekohasekai <contact-sagernet@sekai.icu>
- License: GPL-3.0-or-later.

## Tailscale Go and native dependencies

- Toolchain source: https://github.com/tailscale/go
- Pinned revision: 63ae404c8203317fd3c82d972e5dc8f0fcb425cb
- Toolchain license: BSD-3-Clause, as provided by that repository.

The pinned native modules' `go.mod` and `go.sum` identify dependency versions.
Native release tooling retains the upstream libbox license and the WGC license
with the artifacts. Distributors must also retain notices and provide source
required by their selected native profile and custom extensions. This document
is a provenance summary, not a complete license inventory for every profile.
