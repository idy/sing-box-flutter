#!/usr/bin/env python3
"""Compile representative generated registries and run their host lifecycle tests."""
from __future__ import annotations

import argparse
import subprocess
import sys

sys.dont_write_bytecode = True
from build_core import mobile_overlay, mobile_sources
from build_libbox import bootstrap_go, run, source_checkout
from core_features import canonicalize, generate_registry, resolve_features
from native_common import BuildError, ROOT, build_directory, load_pins, project_lock

PROFILES = {
    "direct": {},
    "tailscale-wireguard": {"endpoints": ["tailscale", "wireguard"], "dns": ["udp", "tcp", "tailscale"]},
    "proxy-clients": {
        "inbounds": ["tun", "mixed", "socks", "http", "naive"],
        "outbounds": ["direct", "selector", "urltest", "socks", "http", "shadowsocks", "snell",
                      "vmess", "vless", "trojan", "ssh", "shadowtls", "anytls", "hysteria", "hysteria2", "tuic"],
        "dns": ["udp", "tcp", "https", "tls", "h3", "quic", "local", "hosts", "fakeip", "mdns", "dhcp"],
        "transports": ["http", "ws", "grpc", "quic", "httpupgrade"],
        "tls": ["utls", "reality", "ech"], "api": ["clash_api", "v2ray_api"],
    },
    "vpn-endpoints": {"endpoints": ["openconnect", "openvpn-client", "openvpn-server"],
                      "dns": ["udp", "openconnect", "openvpn"]},
    "optional-services": {"services": ["api", "ssm-api", "hysteria-realm", "derp", "ccm", "ocm", "oom-killer"],
                          "certificate_providers": ["acme", "tailscale", "cloudflare-origin-ca"]},
}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", choices=("apple", "android", "macos"), default="apple")
    parser.add_argument("--race", action="store_true", help="Enable Go's host race detector")
    args = parser.parse_args()
    try:
        with project_lock():
            bootstrap = build_directory(ROOT / ".build/bootstrap")
            cache = build_directory(ROOT / ".build/cache")
            bootstrap.mkdir(parents=True, exist_ok=True)
            cache.mkdir(parents=True, exist_ok=True)
            pins = load_pins()
            go, env, _ = bootstrap_go(pins, bootstrap, cache)
            source = source_checkout(pins, bootstrap)
            run([go, "mod", "verify"], cwd=source, env=env)
            snapshot = mobile_sources()
            for name, selection in PROFILES.items():
                print(f"PROFILE {name}", flush=True)
                resolved = resolve_features(canonicalize(selection), args.target)
                with mobile_overlay(source, generate_registry(resolved["features"]), snapshot):
                    command = [go, "test"]
                    if args.race:
                        command.append("-race")
                    command += ["-tags", ",".join(resolved["tags"]), "./experimental/libbox"]
                    run(command, cwd=source, env=env)
                    if name == "direct":
                        dependencies = run([go, "list", "-tags", ",".join(resolved["tags"]),
                                            "-deps", "./experimental/libbox"], cwd=source, env=env, capture=True)
                        for forbidden in ("github.com/sagernet/sing-box/experimental/daemon",
                                          "google.golang.org/grpc", "github.com/sagernet/sing-box/include"):
                            if forbidden in dependencies.splitlines():
                                raise BuildError(f"Unselected mobile management dependency: {forbidden}")
            print("Representative profiles compiled and lifecycle/capability tests passed.")
        return 0
    except (BuildError, OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
