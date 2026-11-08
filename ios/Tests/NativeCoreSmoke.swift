// SPDX-License-Identifier: GPL-3.0-or-later
import Darwin
import Foundation
import Libbox

/// Runs inside an iOS simulator so Go's initialization (including Tailscale's
/// toolchain assertion) actually executes. Linking alone cannot establish this.
@main
struct NativeCoreSmoke {
    static func main() throws {
        // simctl captures a pipe; preserve the last completed check even on a crash.
        setbuf(stdout, nil)
        guard CommandLine.arguments.count >= 2 else {
            fatalError("Pass the expected libbox version from native-core.json and optional complete configuration fixture files.")
        }
        guard ProcessInfo.processInfo.environment["TS_PERMIT_TOOLCHAIN_MISMATCH"] != "1" else {
            fatalError("The Tailscale toolchain assertion must not be bypassed for this smoke test.")
        }
        let version = LibboxVersion()
        guard version == CommandLine.arguments[1] else {
            fatalError("The installed libbox version does not match native-core.json.")
        }
        print("libbox version: \(version)")

        // Validation does not start the service or require VPN/App Group consent.
        try NativeCore.check("{\"outbounds\":[{\"type\":\"direct\",\"tag\":\"direct\"}]}")
        print("valid configuration: accepted")

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
        let tailscaleJSON = try JSONSerialization.data(withJSONObject: tailscaleConfig)
        try NativeCore.check(String(decoding: tailscaleJSON, as: UTF8.self))
        print("Tailscale endpoint: registered and configuration accepted")

        // Synthetic keys and documentation-only numeric endpoints. This validates
        // WGC registration in the SAME libbox without connecting or a second core.
        let wgcConfig: [String: Any] = [
            "endpoints": [[
                "type": "wgc", "tag": "wgc",
                "configuration": [
                    "version": 1,
                    "private_key": Data(repeating: 1, count: 32).base64EncodedString(),
                    "addresses": ["10.7.0.2/32"],
                    "peers": [[
                        "id": "exit",
                        "public_key": Data(repeating: 2, count: 32).base64EncodedString(),
                        "endpoint": "192.0.2.1:51820",
                    ]],
                ],
            ]],
            "outbounds": [["type": "direct", "tag": "direct"]],
        ]
        let wgcJSON = try JSONSerialization.data(withJSONObject: wgcConfig)
        try NativeCore.check(String(decoding: wgcJSON, as: UTF8.self))
        print("WGC endpoint: registered and configuration accepted in libbox")

        for path in CommandLine.arguments.dropFirst(2) {
            let url = URL(fileURLWithPath: path)
            let data = try Data(contentsOf: url)
            guard let config = String(data: data, encoding: .utf8) else { throw TunnelFailure.invalidConfig }
            try NativeCore.check(config)
            print("complete converter fixture: \(url.lastPathComponent) accepted")
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
