// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import Libbox
import Network

struct UserTunPathInterface {
    let name: String
    let index: Int32
}

struct UserTunNetworkPath {
    let satisfied: Bool
    let supportsIPv4: Bool
    let supportsIPv6: Bool
    let interfaces: [UserTunPathInterface]
    let expensive: Bool
    let constrained: Bool

    init(_ path: NWPath) {
        satisfied = path.status == .satisfied
        supportsIPv4 = path.supportsIPv4
        supportsIPv6 = path.supportsIPv6
        interfaces = path.availableInterfaces.map { UserTunPathInterface(name: $0.name, index: Int32($0.index)) }
        expensive = path.isExpensive
        constrained = path.isConstrained
    }

    init(satisfied: Bool, supportsIPv4: Bool, supportsIPv6: Bool, interfaces: [UserTunPathInterface], expensive: Bool = false, constrained: Bool = false) {
        self.satisfied = satisfied
        self.supportsIPv4 = supportsIPv4
        self.supportsIPv6 = supportsIPv6
        self.interfaces = interfaces
        self.expensive = expensive
        self.constrained = constrained
    }
}

protocol UserTunPathSource: AnyObject {
    var update: ((UserTunNetworkPath) -> Void)? { get set }
    func start(queue: DispatchQueue)
    func cancel()
}

private final class UserTunNWPathSource: UserTunPathSource {
    private let monitor = NWPathMonitor()
    var update: ((UserTunNetworkPath) -> Void)?
    func start(queue: DispatchQueue) {
        monitor.pathUpdateHandler = { [weak self] in self?.update?(UserTunNetworkPath($0)) }
        monitor.start(queue: queue)
    }
    func cancel() {
        monitor.pathUpdateHandler = nil
        monitor.cancel()
    }
}

/// Supplies only the default uplink. Native sing-tun still creates interfaces,
/// installs routes, enumerates addresses and binds sockets to that uplink.
final class UserTunPathMonitor: NSObject, LibboxStandalonePlatformInterfaceProtocol, @unchecked Sendable {
    private final class Subscription {
        let source: UserTunPathSource
        let listener: LibboxInterfaceUpdateListenerProtocol
        let initial = DispatchSemaphore(value: 0)
        var path: UserTunNetworkPath?
        init(source: UserTunPathSource, listener: LibboxInterfaceUpdateListenerProtocol) {
            self.source = source
            self.listener = listener
        }
    }

    private let lock = NSLock()
    private let queue = DispatchQueue(label: "dev.idy.sing_box_flutter.user-tun.path")
    private let factory: () -> UserTunPathSource
    private let initialTimeout: DispatchTimeInterval
    private var subscription: Subscription?
    private var ownInterfaces = Set<String>()

    init(factory: @escaping () -> UserTunPathSource = { UserTunNWPathSource() }, initialTimeout: DispatchTimeInterval = .seconds(5)) {
        self.factory = factory
        self.initialTimeout = initialTimeout
    }

    func startDefaultInterfaceMonitor(_ listener: LibboxInterfaceUpdateListenerProtocol?) throws {
        guard let listener else { throw TunnelFailure.invalidArguments }
        let current = Subscription(source: factory(), listener: listener)
        lock.lock()
        guard subscription == nil else { lock.unlock(); throw TunnelFailure.busy }
        subscription = current
        lock.unlock()
        current.source.update = { [weak self, weak current] path in
            guard let self, let current else { return }
            self.publish(path, for: current)
            current.initial.signal()
        }
        current.source.start(queue: queue)
        guard current.initial.wait(timeout: .now() + initialTimeout) == .success else {
            reset(current)
            throw TunnelFailure(code: "network_monitor_timeout", message: "The system did not provide an initial network path.")
        }
        lock.lock()
        let active = subscription === current
        lock.unlock()
        guard active else { throw TunnelFailure(code: "network_monitor_stopped", message: "The network path monitor was stopped.") }
    }

    func closeDefaultInterfaceMonitor(_ listener: LibboxInterfaceUpdateListenerProtocol?) throws { reset(nil) }

    func registerMyInterface(_ name: String?) {
        guard let name, !name.isEmpty else { return }
        lock.lock()
        ownInterfaces.insert(name)
        let current = subscription
        lock.unlock()
        // RegisterMyInterface can run under native route/start locks. Refresh
        // asynchronously so its Go listener is never called back synchronously.
        queue.async { [weak self, weak current] in
            guard let self, let current else { return }
            self.lock.lock()
            let path = current.path
            self.lock.unlock()
            if let path { self.publish(path, for: current) }
        }
    }

    private func publish(_ path: UserTunNetworkPath, for current: Subscription) {
        lock.lock()
        guard subscription === current else { lock.unlock(); return }
        current.path = path
        let interface = path.interfaces.first { !ownInterfaces.contains($0.name) }
        lock.unlock()
        if path.satisfied, path.supportsIPv4 || path.supportsIPv6, let interface {
            current.listener.updateDefaultInterface(interface.name, interfaceIndex: interface.index, isExpensive: path.expensive, isConstrained: path.constrained)
        } else {
            current.listener.updateDefaultInterface("", interfaceIndex: -1, isExpensive: false, isConstrained: false)
        }
    }

    private func reset(_ expected: Subscription?) {
        lock.lock()
        guard expected == nil || subscription === expected else { lock.unlock(); return }
        let old = subscription
        subscription = nil
        ownInterfaces.removeAll()
        lock.unlock()
        old?.initial.signal() // Unblock a concurrent initial-path wait immediately.
        old?.source.cancel()
        // In-flight callbacks retain their listener and finish outside all locks.
    }
}
