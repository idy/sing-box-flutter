// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import Libbox

protocol UserTunService: AnyObject {
    func prepare(_ config: String) throws -> UserTunDNSPlan
    func activate(onDNSFailure: @escaping () -> Void) throws
    /// Permanent close is safe concurrently with preparation/activation, including
    /// before either phase has entered Go. It must wait for native teardown.
    func close() throws
}

final class NativeUserTunService: UserTunService {
    private let server: LibboxCommandServer
    private let monitor: UserTunPathMonitor
    private let dns: UserTunDNSController
    private let lock = NSLock()
    private var closed = false
    private var plan: UserTunDNSPlan?

    init() throws {
        monitor = UserTunPathMonitor()
        dns = UserTunDNSController()
        var error: NSError?
        guard let server = LibboxNewStandaloneCommandServerWithMonitor(monitor, &error), error == nil else {
            throw TunnelFailure(code: "core_create_failed", message: "sing-box could not create the macOS user_tun service.")
        }
        self.server = server
    }

    func prepare(_ config: String) throws -> UserTunDNSPlan {
        lock.lock()
        let active = !closed
        lock.unlock()
        guard active else { throw UserTunRuntime.cancelled }
        let native: LibboxStandaloneDNSPlan
        do { native = try server.prepareService(config, overrides: LibboxOverrideOptions()) }
        catch { throw TunnelFailure.invalidConfig }
        var servers: [String] = []
        if let iterator = native.dnsServers() {
            while iterator.hasNext() { servers.append(iterator.next()) }
        }
        let value = UserTunDNSPlan(hasTun: native.hasTun, servers: servers)
        lock.lock()
        defer { lock.unlock() }
        guard !closed else { throw UserTunRuntime.cancelled }
        plan = value
        return value
    }

    func activate(onDNSFailure: @escaping () -> Void) throws {
        lock.lock()
        let value = closed ? nil : plan
        lock.unlock()
        guard let value else { throw UserTunRuntime.cancelled }
        do { try server.activateService() }
        catch { throw UserTunRuntime.startFailed }
        // The plan came from the exact prepared Go configuration. In disabled
        // and manual-route mode it has no servers and performs no DNS writes.
        try dns.install(servers: value.servers, onFailure: onDNSFailure)
    }

    func close() throws {
        lock.lock()
        guard !closed else { lock.unlock(); return }
        closed = true
        plan = nil
        lock.unlock()
        // Always cancel/drain the native server after DNS cleanup, even when
        // removing the temporary resolver fails.
        defer { server.close() }
        try dns.close()
    }

    static func check(_ config: String) throws {
        let service = try NativeUserTunService()
        defer { try? service.close() }
        _ = try service.prepare(config)
    }
}
