#!/usr/bin/env python3
"""Exercise the real helper; --tun must run in a disposable network namespace."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import select
import socket
import subprocess
import tempfile
import time


class Core:
    def __init__(self, helper: str, data: str, *, temporary: bool = False):
        self.process = subprocess.Popen(
            [helper, "--data-dir", data, *(["--temporary-state"] if temporary else [])], stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=0,
        )
        self.sequence = 0
        self.buffer = b""
        self.events: list[dict] = []

    def request(self, method: str, config: dict | None = None):
        self.sequence += 1
        request = {"id": self.sequence, "method": method}
        if config is not None:
            request["config"] = json.dumps(config)
        self.process.stdin.write(json.dumps(request).encode() + b"\n")
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if b"\n" not in self.buffer:
                ready, _, _ = select.select(
                    [self.process.stdout], [], [], max(0, deadline - time.monotonic())
                )
                if not ready:
                    break
                data = os.read(self.process.stdout.fileno(), 65536)
                if not data:
                    raise AssertionError(f"core exited during {method}")
                self.buffer += data
                continue
            line, self.buffer = self.buffer.split(b"\n", 1)
            value = json.loads(line)
            if value.get("type") == "status":
                self.events.append(value)
                continue
            assert value["id"] == self.sequence, value
            return value
        raise AssertionError(f"core timed out during {method}")

    def ok(self, method: str, config: dict | None = None):
        response = self.request(method, config)
        assert "error" not in response, response
        return response["result"]

    def close(self):
        if not self.process.stdin.closed:
            self.process.stdin.close()
        try:
            assert self.process.wait(timeout=15) == 0
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()
            raise


def tx_packets(interface: str) -> int | None:
    # /sys/class/net belongs to the namespace where sysfs was mounted. Proc's
    # self/net view follows this process, including after unshare --net.
    for line in Path("/proc/self/net/dev").read_text().splitlines()[2:]:
        name, _, counters = line.partition(":")
        if name.strip() == interface:
            return int(counters.split()[9])
    return None


def wait_removed(interface: str):
    deadline = time.monotonic() + 5
    while tx_packets(interface) is not None and time.monotonic() < deadline:
        time.sleep(0.05)
    assert tx_packets(interface) is None, f"TUN leaked: {interface}"


def routing_state() -> dict[str, list[str]]:
    """Use namespace-aware netlink tools, preserving duplicate rules/routes."""
    result = {}
    for family in ("-4", "-6"):
        for kind, arguments in (("rules", ["rule", "show"]),
                                ("routes", ["route", "show", "table", "all"])):
            entries = json.loads(subprocess.check_output(
                ["ip", "-json", family, *arguments], text=True))
            result[f"{family}:{kind}"] = sorted(json.dumps(entry, sort_keys=True) for entry in entries)
    return result


def wait_routing_restored(expected: dict[str, list[str]]):
    deadline = time.monotonic() + 5
    while True:
        current = routing_state()
        if current == expected:
            return
        if time.monotonic() >= deadline:
            raise AssertionError(f"Policy routing leaked: before={expected!r}, after={current!r}")
        time.sleep(0.05)


def check_temporary_state_cleanup(helper: str):
    # Test actual helper exit paths rather than having Python's fixture cleanup
    # hide leaked cache/auth state. Persistent state must survive the same EOF.
    with tempfile.TemporaryDirectory(prefix="sing-box-state-lifetime-") as parent:
        outside = Path(parent, "persistent")
        outside.mkdir(mode=0o700)
        sentinel = outside / "identity"
        sentinel.write_text("persistent")
        persistent = Core(helper, str(outside))
        persistent.ok("getInfo")
        persistent.close()
        assert sentinel.read_text() == "persistent"
        for shutdown in ("eof", "sigterm"):
            state = Path(tempfile.mkdtemp(prefix="temporary-", dir=parent))
            core = Core(helper, str(state), temporary=True)
            try:
                core.ok("getInfo")
                cache = state / "cache"
                cache.mkdir()
                (cache / "tailscale-state").write_text("private-auth-state")
                (state / "outside-link").symlink_to(outside, target_is_directory=True)
                if shutdown == "sigterm":
                    core.process.terminate()
                core.close()
                assert not state.exists(), f"temporary state leaked after {shutdown}"
                assert sentinel.read_text() == "persistent"
            finally:
                core.close()
    print("Temporary state removed after EOF/SIGTERM; persistent/outside state preserved")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--helper", required=True)
    parser.add_argument("--tun", action="store_true")
    args = parser.parse_args()
    helper = str(Path(args.helper).resolve(strict=True))
    check_temporary_state_cleanup(helper)
    with tempfile.TemporaryDirectory(prefix="sing-box-linux-smoke-") as data:
        core = Core(helper, data, temporary=True)
        try:
            info = core.ok("getInfo")
            assert info["version"] == "1.14.4", info
            assert len(info["fingerprint"]) == 64, info
            core.ok("initialize")
            assert core.ok("getStatus")["state"] == "disconnected"
            invalid = core.request("checkConfig", {"outbounds": [{"type": "secret-auth-key"}]})
            assert invalid["error"]["code"] == "invalid_config", invalid
            assert "secret-auth-key" not in json.dumps(invalid)

            name = f"sbf{os.getpid()}"
            config = {
                "dns": {"servers": [{"type": "udp", "tag": "dns-direct", "server": "1.1.1.1"}],
                        "final": "dns-direct"},
                "inbounds": [{"type": "tun", "interface_name": name,
                              "address": ["172.29.199.1/30"],
                              "auto_route": True, "stack": "system"}],
                "outbounds": [{"type": "direct", "tag": "direct"}],
                "route": {"auto_detect_interface": True, "final": "direct"},
            }
            core.ok("checkConfig", config)
            permission = core.ok("requestPermission")
            assert permission == args.tun, f"unexpected TUN permission: {permission}"
            if not args.tun:
                denied = core.request("start", config)
                assert denied["error"]["code"] == "permission_denied", denied
                assert tx_packets(name) is None
                print("Linux native validation and unprivileged permission checks passed")
                return

            baseline_routing = routing_state()
            core.ok("start", config)
            assert tx_packets(name) is not None, "connected without a TUN"
            assert core.ok("getStatus")["state"] == "connected"
            assert [e["state"] for e in core.events][-2:] == ["connecting", "connected"]
            # A datagram to a reserved test address must enter the TUN. The direct
            # outbound need not receive a reply, so this requires no public server.
            before = tx_packets(name)
            assert before is not None, "TUN disappeared before packet probe"
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as probe:
                probe.sendto(b"sing-box-flutter-smoke", ("198.18.0.1", 38473))
            deadline = time.monotonic() + 5
            after = tx_packets(name)
            while after == before and time.monotonic() < deadline:
                time.sleep(0.05)
                after = tx_packets(name)
            assert after is not None and after > before, "traffic did not enter the TUN"
            core.ok("stop")
            wait_removed(name)
            wait_routing_restored(baseline_routing)
            assert core.ok("getStatus")["state"] == "disconnected"
            # Parent EOF must also remove the live interface and routing state.
            core.ok("start", config)
            assert tx_packets(name) is not None
            core.close()
            wait_removed(name)
            wait_routing_restored(baseline_routing)
            print("Linux TUN startup, packet capture, stop and parent-exit cleanup passed")
        finally:
            core.close()
            assert not Path(data).exists(), "temporary state leaked after helper exit"


if __name__ == "__main__":
    main()
