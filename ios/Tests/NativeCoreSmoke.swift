// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import Libbox

/// Runs inside an iOS simulator so Go's initialization (including Tailscale's
/// toolchain assertion) actually executes. Linking alone cannot establish this.
@main
struct NativeCoreSmoke {
    static func main() throws {
        guard CommandLine.arguments.count == 2 else {
            fatalError("Pass the selected core-capabilities.json path.")
        }
        guard ProcessInfo.processInfo.environment["TS_PERMIT_TOOLCHAIN_MISMATCH"] != "1" else {
            fatalError("The Tailscale toolchain assertion must not be bypassed for this smoke test.")
        }
        let capabilities = try CoreCapabilities.decode(Data(contentsOf: URL(fileURLWithPath: CommandLine.arguments[1])))
        try NativeCore.verify(capabilities)
        try NativeCore.verifySession(capabilities.fingerprint, capabilities: capabilities)
        do {
            try NativeCore.verifySession("wrong-fingerprint", capabilities: capabilities)
            fatalError("Mismatched saved session accepted")
        } catch let failure as TunnelFailure { precondition(failure.code == "core_mismatch") }
        print("libbox version and runtime fingerprint: verified")

        // Validation does not start the service or require VPN/App Group consent.
        if capabilities.supports("outbounds", "direct") {
            try NativeCore.check("{\"outbounds\":[{\"type\":\"direct\",\"tag\":\"direct\"}]}")
            print("valid configuration: accepted")
        }

        let stateDirectory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: stateDirectory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: stateDirectory) }
        let tailscaleConfig: [String: Any] = [
            "endpoints": [[
                "type": "tailscale", "tag": "tailnet", "hostname": "native-smoke",
                "state_directory": stateDirectory.path,
                "system_interface": false, "accept_routes": false,
            ]],
            "outbounds": [["type": "direct", "tag": "direct"]],
        ]
        if capabilities.supports("endpoints", "tailscale") && capabilities.supports("outbounds", "direct") {
            let tailscaleJSON = try JSONSerialization.data(withJSONObject: tailscaleConfig)
            try NativeCore.check(String(decoding: tailscaleJSON, as: UTF8.self))
            print("Tailscale endpoint: registered and configuration accepted")
        }

        let wgcConfig: [String: Any] = [
            "log": ["disabled": true],
            "inbounds": [[
                "type": "tun", "tag": "tun-in",
                "address": ["172.19.0.1/30", "fd00:7::1/126"],
                "stack": "gvisor", "auto_route": true, "dns_mode": "hijack",
            ]],
            "endpoints": [[
                "type": "wgc", "tag": "wgc", "address": ["10.7.0.2/32"],
                "private_key": Data(repeating: 1, count: 32).base64EncodedString(),
                "peer_public_key": Data(repeating: 2, count: 32).base64EncodedString(),
                "peer_endpoint": "203.0.113.10:51820",
            ]],
            "dns": ["servers": [["type": "udp", "tag": "remote", "server": "1.1.1.1", "detour": "wgc"]], "final": "remote"],
            "route": [
                "auto_detect_interface": true, "default_domain_resolver": "remote",
                "rules": [["port": 53, "action": "hijack-dns"], ["ip_version": 6, "action": "reject"]],
                "final": "wgc",
            ],
        ]
        if capabilities.supports("endpoints", "wgc") && capabilities.supports("inbounds", "tun") && capabilities.supports("dns", "udp") {
            let wgcJSON = try JSONSerialization.data(withJSONObject: wgcConfig)
            try NativeCore.check(String(decoding: wgcJSON, as: UTF8.self))
            print("WGC endpoint: registered using idy/wgc")
        }
        for (group, type) in [("endpoints", "tailscale"), ("outbounds", "vmess"), ("services", "api"), ("certificate_providers", "tailscale")] where !capabilities.supports(group, type) {
            do {
                try NativeCore.check("{\"\(group)\":[{\"type\":\"\(type)\"}]}")
                fatalError("An uncompiled component was accepted")
            } catch let failure as TunnelFailure { precondition(failure.code == "invalid_config") }
        }

        let secret = "native-smoke-private-auth-sentinel"
        do {
            try NativeCore.check("{\"outbounds\":[{\"type\":\"invalid-smoke-outbound\",\"password\":\"\(secret)\"}]}")
            fatalError("Invalid configuration was accepted.")
        } catch let failure as TunnelFailure {
            guard failure.code == "invalid_config", !failure.message.contains(secret) else {
                fatalError("Configuration validation did not return a credential-safe platform error.")
            }
        }
        print("invalid configuration: rejected with credential-safe error")

        let event = TunnelStatus("error", message: String(repeating: "a", count: 1000)).event
        guard event["type"] as? String == "status",
              event["state"] as? String == "error",
              (event["message"] as? String)?.count == 240 else {
            fatalError("Status payload does not conform to the bounded channel event shape.")
        }
        print("status payload: bounded and conforms to channel event shape")
    }
}
