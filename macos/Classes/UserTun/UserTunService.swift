// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import Libbox

protocol UserTunService: AnyObject {
    func prepare(_ config: String) throws -> UserTunDNSPlan
    func activate(onDNSFailure: @escaping () -> Void) throws
    /// Permanent close is safe concurrently with preparation/activation, including
    /// before either phase has entered Go. It must wait for native teardown and
    /// preserve any failure on repeated calls; uncertain cleanup is not retried.
    func close() throws
}

final class NativeUserTunService: UserTunService {
    private let server: LibboxCommandServer
    private let monitor: UserTunPathMonitor
    private let dns: UserTunDNSController
    private let closeNative: () throws -> Void
    private let lock = NSCondition()
    private var closed = false
    private var closeFinished = false
    private var closeFailure: TunnelFailure?
    private var plan: UserTunDNSPlan?

    init(dns: UserTunDNSController = UserTunDNSController(), closeNative: @escaping (LibboxCommandServer) throws -> Void = { try $0.closeWithError() }) throws {
        monitor = UserTunPathMonitor()
        self.dns = dns
        var error: NSError?
        guard let server = LibboxNewStandaloneCommandServerWithMonitor(monitor, &error), error == nil else {
            throw TunnelFailure(code: "core_create_failed", message: "sing-box could not create the macOS user_tun service.")
        }
        self.server = server
        self.closeNative = { try closeNative(server) }
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
        if closed {
            while !closeFinished { lock.wait() }
            let failure = closeFailure
            lock.unlock()
            if let failure { throw failure }
            return
        }
        closed = true
        plan = nil
        lock.unlock()
        // Mark closed before entering either subsystem and never hold our state
        // lock across Go. Both cleanups must run even if the other one fails.
        var failure: TunnelFailure?
        do { try dns.close() } catch { failure = UserTunRuntime.stopFailed }
        do { try closeNative() } catch { failure = UserTunRuntime.stopFailed }
        lock.lock()
        closeFailure = failure
        closeFinished = true
        lock.broadcast()
        lock.unlock()
        if let failure { throw failure }
    }

    static func check(_ config: String) throws {
        let service = try NativeUserTunService()
        var preparationFailure: Error?
        do { _ = try service.prepare(config) } catch { preparationFailure = error }
        // A cleanup failure takes precedence: another engine cannot safely use
        // these native resources even when preparation itself was invalid.
        try service.close()
        if let preparationFailure { throw preparationFailure }
    }
}
