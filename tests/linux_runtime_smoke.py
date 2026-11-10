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
    def __init__(self, helper: str, data: str):
        self.process = subprocess.Popen(
            [helper, "--data-dir", data], stdin=subprocess.PIPE,
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


def wait_removed(interface: Path):
    deadline = time.monotonic() + 5
    while interface.exists() and time.monotonic() < deadline:
        time.sleep(0.05)
    assert not interface.exists(), f"TUN leaked: {interface.name}"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--helper", required=True)
    parser.add_argument("--tun", action="store_true")
    args = parser.parse_args()
    helper = str(Path(args.helper).resolve(strict=True))
    with tempfile.TemporaryDirectory(prefix="sing-box-linux-smoke-") as data:
        core = Core(helper, data)
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
            interface = Path("/sys/class/net") / name
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
                assert not interface.exists()
                print("Linux native validation and unprivileged permission checks passed")
                return

            core.ok("start", config)
            assert interface.exists(), "connected without a TUN"
            assert core.ok("getStatus")["state"] == "connected"
            assert [e["state"] for e in core.events][-2:] == ["connecting", "connected"]
            # A datagram to a reserved test address must enter the TUN. The direct
            # outbound need not receive a reply, so this requires no public server.
            counter = interface / "statistics" / "tx_packets"
            before = int(counter.read_text())
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as probe:
                probe.sendto(b"sing-box-flutter-smoke", ("198.18.0.1", 38473))
            deadline = time.monotonic() + 5
            while int(counter.read_text()) == before and time.monotonic() < deadline:
                time.sleep(0.05)
            assert int(counter.read_text()) > before, "traffic did not enter the TUN"
            core.ok("stop")
            wait_removed(interface)
            assert core.ok("getStatus")["state"] == "disconnected"
            # Parent EOF must also remove the live interface and routing state.
            core.ok("start", config)
            assert interface.exists()
            core.close()
            wait_removed(interface)
            print("Linux TUN startup, packet capture, stop and parent-exit cleanup passed")
        finally:
            core.close()


if __name__ == "__main__":
    main()
