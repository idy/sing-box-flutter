// SPDX-License-Identifier: GPL-3.0-or-later
import FlutterMacOS
import Foundation
import Libbox

private func userTunRequire(_ condition: Bool, _ message: String = "Fake user_tun assertion failed.") { precondition(condition, message) }

/// Exercises the lifecycle and native boundaries with fakes only. No test opens
/// a utun, installs routes, starts NWPathMonitor or writes system DNS settings.
enum UserTunRuntimeSmoke {
    private final class Reply: @unchecked Sendable {
        let lock = NSLock()
        var continuation: CheckedContinuation<Any?, Never>?
        init(_ continuation: CheckedContinuation<Any?, Never>) { self.continuation = continuation }
        func complete(_ value: Any?) {
            lock.lock()
            let pending = continuation
            continuation = nil
            lock.unlock()
            pending?.resume(returning: value)
        }
    }

    private final class Statuses {
        private let lock = NSLock()
        private var values: [String] = []
        func append(_ status: TunnelStatus) { lock.lock(); values.append(status.state); lock.unlock() }
        var states: [String] { lock.lock(); defer { lock.unlock() }; return values }
    }

    private final class FakeService: UserTunService {
        let prepareEntered = DispatchSemaphore(value: 0)
        let activateEntered = DispatchSemaphore(value: 0)
        let closeEntered = DispatchSemaphore(value: 0)
        let prepareGate = DispatchSemaphore(value: 0)
        let activateGate = DispatchSemaphore(value: 0)
        let closeGate = DispatchSemaphore(value: 0)
        let blockPrepare: Bool
        let blockActivate: Bool
        let blockClose: Bool
        let holdActivationAfterClose: Bool
        let hasTun: Bool
        let prepareFails: Bool
        let activateFails: Bool
        private let lock = NSLock()
        private var closed = false
        private var closes = 0
        private var dnsFailure: (() -> Void)?

        init(blockPrepare: Bool = false, blockActivate: Bool = false, blockClose: Bool = false, holdActivationAfterClose: Bool = false, hasTun: Bool = true, prepareFails: Bool = false, activateFails: Bool = false) {
            self.blockPrepare = blockPrepare
            self.blockActivate = blockActivate
            self.blockClose = blockClose
            self.holdActivationAfterClose = holdActivationAfterClose
            self.hasTun = hasTun
            self.prepareFails = prepareFails
            self.activateFails = activateFails
        }
        var closeCount: Int { lock.lock(); defer { lock.unlock() }; return closes }
        func prepare(_ config: String) throws -> UserTunDNSPlan {
            prepareEntered.signal()
            if blockPrepare { userTunRequire(prepareGate.wait(timeout: .now() + 5) == .success) }
            lock.lock(); let stopped = closed; lock.unlock()
            if stopped { throw UserTunRuntime.cancelled }
            if prepareFails { throw TunnelFailure.invalidConfig }
            return UserTunDNSPlan(hasTun: hasTun, servers: ["172.19.0.2"])
        }
        func activate(onDNSFailure: @escaping () -> Void) throws {
            lock.lock(); dnsFailure = onDNSFailure; lock.unlock()
            activateEntered.signal()
            if blockActivate { userTunRequire(activateGate.wait(timeout: .now() + 5) == .success) }
            if activateFails { throw NSError(domain: "private-native-diagnostic", code: 1) }
            // Deliberately allow a stale success: the runtime, rather than the
            // fake, must suppress connected after stop wins the generation.
        }
        func close() throws {
            lock.lock(); closed = true; closes += 1; lock.unlock()
            closeEntered.signal()
            prepareGate.signal()
            if !holdActivationAfterClose { activateGate.signal() }
            if blockClose { userTunRequire(closeGate.wait(timeout: .now() + 5) == .success) }
        }
        func failDNS() { lock.lock(); let callback = dnsFailure; lock.unlock(); callback?() }
    }

    private static func call(_ runtime: UserTunRuntime, _ method: String, _ arguments: [String: Any] = [:]) async -> Any? {
        await withCheckedContinuation { continuation in
            let reply = Reply(continuation)
            DispatchQueue.global().asyncAfter(deadline: .now() + 4) {
                reply.complete(FlutterError(code: "fixture_timeout", message: "The runtime did not answer \(method).", details: nil))
            }
            runtime.handle(FlutterMethodCall(methodName: method, arguments: arguments)) { reply.complete($0) }
        }
    }

    private static func signalled(_ semaphore: DispatchSemaphore) async -> Bool {
        await withCheckedContinuation { continuation in
            DispatchQueue.global().async { continuation.resume(returning: semaphore.wait(timeout: .now() + 3) == .success) }
        }
    }

    private static func eventually(_ predicate: () -> Bool) async -> Bool {
        for _ in 0..<300 {
            if predicate() { return true }
            try? await Task.sleep(nanoseconds: 10_000_000)
        }
        return false
    }

    private static func runtime(_ factory: @escaping () -> UserTunService, privileged: Bool = true) async -> (UserTunRuntime, Statuses) {
        let runtime = UserTunRuntime(dependencies: .init(version: { "fixture" }, setup: {}, check: { _ in }, makeService: factory, isPrivileged: { privileged }))
        let statuses = Statuses()
        _ = runtime.addObserver { statuses.append($0) }
        userTunRequire(await call(runtime, "initialize") == nil)
        return (runtime, statuses)
    }

    private static let startArguments: [String: Any] = ["config": "fixture-private-config", "name": "Fixture"]

    static func run() async throws {
        for (service, expected) in [(FakeService(prepareFails: true), "invalid_config"), (FakeService(hasTun: false), "missing_tun")] {
            let (runtime, statuses) = await runtime({ service })
            let error = await call(runtime, "start", startArguments) as? FlutterError
            userTunRequire(error?.code == expected)
            userTunRequire(!statuses.states.contains("connecting") && !statuses.states.contains("connected"))
            _ = await call(runtime, "stop")
            userTunRequire(service.closeCount == 1)
        }
        let (denied, _) = await runtime({ preconditionFailure("permission check must precede construction") }, privileged: false)
        userTunRequire((await call(denied, "start") as? FlutterError)?.code == "permission_required")

        let preparing = FakeService(blockPrepare: true)
        let (preparingRuntime, preparingStatuses) = await runtime({ preparing })
        let pendingStart = Task { await call(preparingRuntime, "start", startArguments) }
        userTunRequire(await signalled(preparing.prepareEntered))
        userTunRequire((await call(preparingRuntime, "getStatus") as? [String: Any])?["state"] as? String == "disconnected")
        userTunRequire(await call(preparingRuntime, "stop") == nil)
        userTunRequire((await pendingStart.value as? FlutterError)?.code == "operation_cancelled")
        userTunRequire(!preparingStatuses.states.contains("connected") && !preparingStatuses.states.contains("error"))

        let activating = FakeService(blockActivate: true)
        let (activeRuntime, activeStatuses) = await runtime({ activating })
        // The start future completes while activation is still blocked, allowing
        // the checked-in example to expose its Disconnect button.
        userTunRequire(await call(activeRuntime, "start", startArguments) == nil)
        userTunRequire(await signalled(activating.activateEntered))
        userTunRequire((await call(activeRuntime, "getStatus") as? [String: Any])?["state"] as? String == "connecting")
        userTunRequire(await call(activeRuntime, "stop") == nil)
        userTunRequire(activeStatuses.states.last == "disconnected" && !activeStatuses.states.contains("connected") && !activeStatuses.states.contains("error"))

        let stale = FakeService(blockActivate: true, blockClose: true, holdActivationAfterClose: true)
        let replacement = FakeService()
        var generation = 0
        let (staleRuntime, staleStatuses) = await runtime({ generation += 1; return generation == 1 ? stale : replacement })
        userTunRequire(await call(staleRuntime, "start", startArguments) == nil)
        userTunRequire(await signalled(stale.activateEntered))
        let stopping = Task { await call(staleRuntime, "stop") }
        userTunRequire(await signalled(stale.closeEntered))
        userTunRequire((await call(staleRuntime, "start", startArguments) as? FlutterError)?.code == "busy")
        stale.closeGate.signal()
        // Wait until Close has completed; the stale startup worker still owns
        // native teardown until its own result has drained.
        userTunRequire((await call(staleRuntime, "getStatus") as? [String: Any])?["state"] as? String == "disconnecting")
        userTunRequire((await call(staleRuntime, "start", startArguments) as? FlutterError)?.code == "busy")
        stale.activateGate.signal()
        userTunRequire(await stopping.value == nil)
        userTunRequire(!staleStatuses.states.contains("connected") && !staleStatuses.states.contains("error"))
        userTunRequire(await call(staleRuntime, "start", startArguments) == nil)
        userTunRequire(await eventually { staleStatuses.states.last == "connected" })
        replacement.failDNS()
        userTunRequire(await eventually { staleStatuses.states.last == "error" && replacement.closeCount == 1 })
        userTunRequire(replacement.closeCount == 1)
        _ = await call(staleRuntime, "stop")

        let failed = FakeService(activateFails: true)
        let (failedRuntime, failedStatuses) = await runtime({ failed })
        userTunRequire(await call(failedRuntime, "start", startArguments) == nil)
        userTunRequire(await eventually { failedStatuses.states.last == "error" && failed.closeCount == 1 })
        userTunRequire(!failedStatuses.states.contains("connected") && failed.closeCount == 1)
        _ = await call(failedRuntime, "stop")

        try await dnsTests()
        try await pathTests()
        print("Fake user_tun runtime: responsive stop/status, acceptance, generations, DNS ownership/recovery and IPv6 paths passed without host network writes.")
    }

    private final class FakeDNSStore: UserTunDNSStore {
        private let lock = NSLock()
        private var entries: [String: [String: Any]] = [:]
        private var owned: [String: [String: Any]] = [:]
        private var additions: [String] = []
        private var removals: [String] = []
        private var closes = 0
        private var changed: (() -> Void)?
        private var failAdd = false
        var failRemove = false
        func setChanged(_ callback: @escaping () -> Void) { lock.lock(); changed = callback; lock.unlock() }
        var addKeys: [String] { lock.lock(); defer { lock.unlock() }; return additions }
        var removeKeys: [String] { lock.lock(); defer { lock.unlock() }; return removals }
        var closeCount: Int { lock.lock(); defer { lock.unlock() }; return closes }
        func failNextAdd() { lock.lock(); failAdd = true; lock.unlock() }
        func addTemporary(key: String, value: [String: Any]) throws {
            lock.lock()
            additions.append(key)
            entries[key] = value
            owned[key] = value
            let fail = failAdd
            failAdd = false
            lock.unlock()
            if fail { throw UserTunDNSController.configurationFailure }
        }
        func value(forKey key: String) -> [String: Any]? { lock.lock(); defer { lock.unlock() }; return entries[key] }
        func remove(key: String) throws {
            lock.lock()
            defer { lock.unlock() }
            if failRemove { throw UserTunDNSController.configurationFailure }
            removals.append(key)
            entries.removeValue(forKey: key)
            owned.removeValue(forKey: key)
        }
        func close() {
            lock.lock()
            closes += 1
            for (key, expected) in owned {
                if let current = entries[key], NSDictionary(dictionary: current).isEqual(to: expected) { entries.removeValue(forKey: key) }
            }
            owned.removeAll()
            lock.unlock()
        }
        func restart() {
            lock.lock(); entries.removeAll(); owned.removeAll(); let callback = changed; lock.unlock()
            callback?()
        }
        func replace(key: String, value: [String: Any]) {
            lock.lock(); entries[key] = value; let callback = changed; lock.unlock()
            callback?()
        }
        func notify() { lock.lock(); let callback = changed; lock.unlock(); callback?() }
    }

    private static func dnsTests() async throws {
        let store = FakeDNSStore()
        let registration = UserTunDNSController { _, _, callback in store.setChanged(callback); return store }
        try registration.install(servers: ["172.19.0.2", "fdfe:dcba::2"]) { preconditionFailure("healthy resolver failed") }
        userTunRequire(store.addKeys.count == 1)
        let key = store.addKeys[0]
        let value = store.value(forKey: key)!
        userTunRequire(key.hasPrefix("State:/Network/Service/") && key.hasSuffix("/DNS"))
        userTunRequire(value["SupplementalMatchDomains"] as? [String] == [""] && value["SearchOrder"] as? Int == 100000)
        userTunRequire(value["ServerAddresses"] as? [String] == ["172.19.0.2", "fdfe:dcba::2"])
        store.restart()
        userTunRequire(await eventually { store.addKeys.count == 2 })
        try registration.close()
        userTunRequire(store.removeKeys == [key] && store.closeCount == 1 && store.value(forKey: key) == nil)
        store.notify()
        userTunRequire(store.addKeys.count == 2)

        let disabled = UserTunDNSController { _, _, _ in preconditionFailure("disabled DNS created a store") }
        try disabled.install(servers: []) {}
        try disabled.close()

        let secondStore = FakeDNSStore()
        let second = UserTunDNSController { _, _, callback in secondStore.setChanged(callback); return secondStore }
        let failed = DispatchSemaphore(value: 0)
        try second.install(servers: ["172.19.0.2"]) { failed.signal() }
        userTunRequire(secondStore.addKeys[0] != key)
        secondStore.failNextAdd()
        secondStore.restart()
        userTunRequire(await signalled(failed))
        try second.close()
        userTunRequire(secondStore.closeCount == 1 && secondStore.value(forKey: secondStore.addKeys[0]) == nil)

        let partialStore = FakeDNSStore()
        partialStore.failNextAdd()
        let partial = UserTunDNSController { _, _, _ in partialStore }
        do { try partial.install(servers: ["172.19.0.2"]) {}; preconditionFailure("partial write succeeded") }
        catch { userTunRequire(partialStore.closeCount == 1 && partialStore.value(forKey: partialStore.addKeys[0]) == nil) }
        try partial.close()

        let foreignStore = FakeDNSStore()
        let foreign = UserTunDNSController { _, _, callback in foreignStore.setChanged(callback); return foreignStore }
        let replaced = DispatchSemaphore(value: 0)
        try foreign.install(servers: ["172.19.0.2"]) { replaced.signal() }
        let foreignKey = foreignStore.addKeys[0]
        foreignStore.replace(key: foreignKey, value: ["ServerAddresses": ["192.0.2.53"]])
        userTunRequire(await signalled(replaced))
        try foreign.close()
        userTunRequire(foreignStore.removeKeys.isEmpty && foreignStore.value(forKey: foreignKey)?["ServerAddresses"] as? [String] == ["192.0.2.53"])

        let removalStore = FakeDNSStore()
        let removal = UserTunDNSController { _, _, _ in removalStore }
        try removal.install(servers: ["172.19.0.2"]) {}
        removalStore.failRemove = true
        do { try removal.close(); preconditionFailure("remove failure hidden") }
        catch { userTunRequire(removalStore.closeCount == 1 && removalStore.value(forKey: removalStore.addKeys[0]) == nil) }
    }

    private final class FakePathSource: UserTunPathSource {
        private let lock = NSLock()
        private var handler: ((UserTunNetworkPath) -> Void)?
        private var queue: DispatchQueue?
        let first: UserTunNetworkPath
        init(_ first: UserTunNetworkPath) { self.first = first }
        var update: ((UserTunNetworkPath) -> Void)? {
            get { lock.lock(); defer { lock.unlock() }; return handler }
            set { lock.lock(); handler = newValue; lock.unlock() }
        }
        func start(queue: DispatchQueue) { lock.lock(); self.queue = queue; lock.unlock(); emit(first) }
        func cancel() {}
        func emit(_ path: UserTunNetworkPath) {
            lock.lock(); let queue = queue; let callback = handler; lock.unlock()
            queue?.async { callback?(path) }
        }
    }

    private final class PathListener: NSObject, LibboxInterfaceUpdateListenerProtocol, @unchecked Sendable {
        private let lock = NSLock()
        private var indices: [Int32] = []
        var updates: [Int32] { lock.lock(); defer { lock.unlock() }; return indices }
        func updateDefaultInterface(_ interfaceName: String?, interfaceIndex: Int32, isExpensive: Bool, isConstrained: Bool) {
            lock.lock(); indices.append(interfaceIndex); lock.unlock()
        }
        func updateNetworkPath(_ networkPath: String?) {}
    }

    private static func pathTests() async throws {
        let ipv6 = UserTunNetworkPath(satisfied: true, supportsIPv4: false, supportsIPv6: true, interfaces: [
            .init(name: "own-dynamic-tunnel", index: 600), .init(name: "unusual-v6-uplink", index: 601),
        ])
        let source = FakePathSource(ipv6)
        let monitor = UserTunPathMonitor(factory: { source }, initialTimeout: .seconds(1))
        let listener = PathListener()
        let started: Bool = await withCheckedContinuation { continuation in
            DispatchQueue.global().async {
                do { try monitor.startDefaultInterfaceMonitor(listener); continuation.resume(returning: true) }
                catch { continuation.resume(returning: false) }
            }
        }
        userTunRequire(started && listener.updates.last == 600)
        monitor.registerMyInterface("own-dynamic-tunnel")
        userTunRequire(await eventually { listener.updates.last == 601 })
        monitor.registerMyInterface("unusual-v6-uplink")
        userTunRequire(await eventually { listener.updates.last == -1 })
        let late = source.update
        try monitor.closeDefaultInterfaceMonitor(listener)
        let count = listener.updates.count
        late?(ipv6)
        userTunRequire(listener.updates.count == count)
    }
}
