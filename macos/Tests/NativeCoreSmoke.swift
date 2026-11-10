// SPDX-License-Identifier: GPL-3.0-or-later
import Darwin
import FlutterMacOS
import Foundation
import Libbox

/// Exercises the actual Go runtime without creating a utun or changing routes/DNS.
@main
struct NativeCoreSmoke {
    static func call(_ plugin: SingBoxFlutterPlugin, _ method: String, _ arguments: [String: Any] = [:]) async -> Any? {
        await withCheckedContinuation { continuation in
            plugin.handle(FlutterMethodCall(methodName: method, arguments: arguments)) { continuation.resume(returning: $0) }
        }
    }

    static func main() async throws {
        guard ProcessInfo.processInfo.environment["TS_PERMIT_TOOLCHAIN_MISMATCH"] != "1" else {
            fatalError("Do not bypass the pinned Go toolchain assertion.")
        }
        let capabilities = try CoreCapabilities.load()
        try NativeCore.verify(capabilities)
        precondition(capabilities.supports("outbounds", "direct") && capabilities.supports("dns", "udp"), "Smoke core must include direct and UDP DNS.")
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        try NativeCore.setup(directory: directory, crashReportSource: "macOS native smoke")
        // Explicit UDP avoids the implicit local resolver, which intentionally
        // is not compiled into the minimal WGC profile used by CI.
        let config = """
        {"log":{"disabled":true},"dns":{"servers":[{"type":"udp","tag":"smoke-dns","server":"127.0.0.1","server_port":9}],"final":"smoke-dns"},"outbounds":[{"type":"direct","tag":"direct"}]}
        """
        try NativeCore.check(config)
        // Exercise the same monitor-only adapter as the Flutter backend. The
        // legacy constructor uses a different, native route-table monitor.
        let monitor = UserTunPathMonitor()
        var error: NSError?
        guard let server = LibboxNewStandaloneCommandServerWithMonitor(monitor, &error), error == nil else {
            fatalError("Standalone command server creation failed.")
        }
        defer { server.close() }
        let prepared = try server.prepareService(config, overrides: LibboxOverrideOptions())
        precondition(!prepared.hasTun && prepared.dnsMode == LibboxDNSModeDisabled)
        print("Monitored native service prepared; activating.")
        try server.activateService()
        print("Monitored native service activated; closing before restart.")
        try server.closeService()
        print("Monitored native service closed; restarting.")
        try server.startOrReloadService(config, options: LibboxOverrideOptions())
        try server.closeService()
        server.close()
        print("Actual macOS Go runtime: metadata verified, standalone service started, stopped and restarted with native networking and host path monitoring.")

        let secret = "native-smoke-private-auth-sentinel"
        do {
            try NativeCore.check("{\"outbounds\":[{\"type\":\"invalid-native-smoke\",\"password\":\"\(secret)\"}]}")
            fatalError("Invalid core configuration accepted.")
        } catch let failure as TunnelFailure {
            precondition(failure.code == "invalid_config" && !failure.message.contains(secret))
        }

        let first = SingBoxFlutterPlugin()
        let initialization = await call(first, "initialize")
        precondition(initialization == nil)
        let permission = await call(first, "requestPermission") as? Bool
        precondition(permission == (geteuid() == 0))
        if geteuid() != 0 {
            let denied = await call(first, "start", ["config": "invalid-private-config", "name": "Smoke"]) as? FlutterError
            precondition(denied?.code == "permission_required")
            precondition(denied?.message?.contains("root") == true && denied?.message?.contains("unsandboxed") == true)
            precondition(denied?.message?.contains("invalid-private-config") == false)
            print("Unprivileged host: permission false; start rejected with fixed permission_required error.")
        }
        _ = first.onCancel(withArguments: nil)
        let second = SingBoxFlutterPlugin()
        let current = await call(second, "getStatus") as? [String: Any]
        precondition(current?["type"] as? String == "status" && current?["state"] as? String == "disconnected")
        let validation = await call(second, "checkConfig", ["config": config])
        precondition(validation == nil)
        print("Flutter channel contract: initialized runtime survives a new plugin instance and event cancellation; native validation is credential-safe.")
        try await UserTunRuntimeSmoke.run()
    }
}
