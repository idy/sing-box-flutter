# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

This record describes the project website; the plugin itself targets Android and iOS.

## Stack

Static HTML, CSS and JavaScript, following the user's request for an HTML explanation and a project page. GitHub Pages is the proposed host for `sing-box-flutter.idy.dev`.

## Users and purpose

Flutter developers evaluating and integrating sing-box VPN capabilities. Explain host-selected native compilation, provide working setup commands, and direct readers to detailed platform documentation and source.

## Product truth

The host application's `pubspec.yaml` selects constructors and required dependencies. An explicit Dart CLI command builds with the pinned, verified Tailscale Go fork. Go compilation occurs on the build machine. Selected artifacts stay in the host's `.dart_tool/sing_box_flutter/`. Android uses VpnService; only the iOS Packet Tunnel extension links Go. The plugin retains its Flutter control API.

## Evidence and constraints

Use `README.md`, `doc/core-features.md`, `doc/native-build.md` and the three checked-in example profiles. The approved README overview supplies the existing blue/slate identity and measured comparison. Sizes compare the minimal WGC profile with cached standard binaries; they are not download or memory measurements. Do not claim universal protocol test coverage, device forwarding verification, or community exclusivity. Version 0.3.0 is in preparation until actually published. Native SDK binaries are not website or release downloads.

## Open deployment state

At the start of this website task GitHub Pages was disabled and the custom domain had no CNAME record. The public homepage must not be changed in the package until the website is live and verified.
