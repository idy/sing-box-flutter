# Third-party notices

`sing_box_flutter` is an independent Flutter integration. It is not affiliated
with or endorsed by SagerNet or Tailscale.

## sing-box and libbox

- Source: https://github.com/SagerNet/sing-box
- Pinned version: 1.14.4
- Pinned revision: c005f35300ce76427dc285481ec5695f5b179cd2
- Copyright (C) 2022 by nekohasekai <contact-sagernet@sekai.icu>
- License: GNU General Public License, version 3 or (at your option) any later
  version. The license text is included in `LICENSE`.

The upstream README additionally states:

> In addition, no derivative work may use the name or imply association with this
> application without prior consent.

Native libbox artifacts are built separately. Applications distributing them must
retain applicable notices and provide corresponding source, including their own
modifications and the source of linked components as required by the licenses.
The original core is identified here for provenance; this package does not claim
to be an official sing-box application.

## Official platform clients

The Android and Apple native integrations use the official platform clients as
implementation references. Files adapted from upstream retain source notices.

- https://github.com/SagerNet/sing-box-for-android
- https://github.com/SagerNet/sing-box-for-apple
- Copyright (C) 2022 by nekohasekai <contact-sagernet@sekai.icu>
- License: GPL-3.0-or-later

## Tailscale Go

- Source: https://github.com/tailscale/go
- Pinned revision: 63ae404c8203317fd3c82d972e5dc8f0fcb425cb
- License: BSD-3-Clause, as provided by that repository.

Tailscale Go is a build toolchain. Its binaries are cached locally and are not
included in the Dart package. Its source and release artifacts remain available
from the upstream repository.

## Transitive native dependencies

The pinned sing-box source and its `go.mod` and `go.sum` identify transitive
native dependencies, including the SagerNet Tailscale fork. These dependencies
retain their own copyright and license notices. A distributor must review and
ship the notices required by the native components they include; this document
is not a replacement for the transitive dependencies' license files.
