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
        let closeFails: Bool
        private let lock = NSLock()
        private var closed = false
        private var closes = 0
        private var dnsFailure: (() -> Void)?

        init(blockPrepare: Bool = false, blockActivate: Bool = false, blockClose: Bool = false, holdActivationAfterClose: Bool = false, hasTun: Bool = true, prepareFails: Bool = false, activateFails: Bool = false, closeFails: Bool = false) {
            self.blockPrepare = blockPrepare
            self.blockActivate = blockActivate
            self.blockClose = blockClose
            self.holdActivationAfterClose = holdActivationAfterClose
            self.hasTun = hasTun
            self.prepareFails = prepareFails
            self.activateFails = activateFails
            self.closeFails = closeFails
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
            if closeFails { throw NSError(domain: "private-close-config-credential", code: 1) }
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

    private static func runtime(_ factory: @escaping () -> UserTunService, privileged: Bool = true, check: @escaping (String) throws -> Void = { _ in }) async -> (UserTunRuntime, Statuses) {
        let runtime = UserTunRuntime(dependencies: .init(version: { "fixture" }, setup: {}, check: check, makeService: factory, isPrivileged: { privileged }))
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

        await checkTests()
        await closeFailureTests()
        try await nativeCloseTests()
        try await dnsTests()
        try await pathTests()
        print("Fake user_tun runtime: responsive stop/status, exclusive validation, permanent close failures, replacement guards, generations, DNS ownership/recovery and IPv6 paths passed without host network writes.")
    }

    private static func requireStopFailure(_ value: Any?) {
        let error = value as? FlutterError
        userTunRequire(error?.code == UserTunRuntime.stopFailed.code)
        userTunRequire(error?.message == UserTunRuntime.stopFailed.message && error?.details == nil)
    }

    private final class Completion {
        private let lock = NSLock()
        private var done = false
        func finish() { lock.lock(); done = true; lock.unlock() }
        var finished: Bool { lock.lock(); defer { lock.unlock() }; return done }
    }

    private static func closeFailureTests() async {
        let connected = FakeService(closeFails: true)
        var constructions = 0
        var checks = 0
        let (connectedRuntime, connectedStatuses) = await runtime({ constructions += 1; return connected }, check: { _ in checks += 1 })
        userTunRequire(await call(connectedRuntime, "start", startArguments) == nil)
        userTunRequire(await eventually { connectedStatuses.states.last == "connected" })
        requireStopFailure(await call(connectedRuntime, "stop"))
        for _ in 0..<2 { requireStopFailure(await call(connectedRuntime, "stop")) }
        userTunRequire(await call(connectedRuntime, "initialize") == nil)
        requireStopFailure(await call(connectedRuntime, "start", startArguments))
        requireStopFailure(await call(connectedRuntime, "checkConfig", ["config": "valid"]))
        let status = await call(connectedRuntime, "getStatus") as? [String: Any]
        userTunRequire(status?["state"] as? String == "error" && status?["message"] as? String == UserTunRuntime.stopFailed.message)
        userTunRequire(constructions == 1 && checks == 0 && connected.closeCount == 1)

        // Native close can fail while startup still owns its worker. Report the
        // sanitized error promptly, but keep every stop reply and replacement
        // behind the same two-worker barrier, including repeated stop requests.
        let activating = FakeService(blockActivate: true, blockClose: true, holdActivationAfterClose: true, closeFails: true)
        let (activatingRuntime, activatingStatuses) = await runtime({ activating })
        userTunRequire(await call(activatingRuntime, "start", startArguments) == nil)
        userTunRequire(await signalled(activating.activateEntered))
        let firstCompletion = Completion()
        let firstStop = Task { let value = await call(activatingRuntime, "stop"); firstCompletion.finish(); return value }
        userTunRequire(await signalled(activating.closeEntered))
        userTunRequire((await call(activatingRuntime, "getStatus") as? [String: Any])?["state"] as? String == "disconnecting")
        userTunRequire((await call(activatingRuntime, "start", startArguments) as? FlutterError)?.code == "busy")
        activating.closeGate.signal()
        userTunRequire(await eventually { activatingStatuses.states.last == "error" })
        userTunRequire(!firstCompletion.finished, "stop returned before the startup worker drained")
        userTunRequire((await call(activatingRuntime, "start", startArguments) as? FlutterError)?.code == "busy")
        let secondStop = Task { await call(activatingRuntime, "stop") }
        activating.activateGate.signal()
        requireStopFailure(await firstStop.value)
        requireStopFailure(await secondStop.value)
        requireStopFailure(await call(activatingRuntime, "stop"))
        requireStopFailure(await call(activatingRuntime, "start", startArguments))
        userTunRequire(!activatingStatuses.states.contains("connected") && activating.closeCount == 1)

        // Preparation errors still answer the original start with its own
        // failure, then poison replacement only if the required cleanup fails.
        let preparing = FakeService(prepareFails: true, closeFails: true)
        let (preparingRuntime, preparingStatuses) = await runtime({ preparing })
        userTunRequire((await call(preparingRuntime, "start", startArguments) as? FlutterError)?.code == "invalid_config")
        userTunRequire(await eventually { preparingStatuses.states.last == "error" })
        requireStopFailure(await call(preparingRuntime, "stop"))
        requireStopFailure(await call(preparingRuntime, "start", startArguments))
        userTunRequire(!preparingStatuses.states.contains("connecting") && preparing.closeCount == 1)

        // Validation owns the same native directories and must retain its close
        // failure too, even though no tunnel session has been published.
        var validationCalls = 0
        let (checkingRuntime, checkingStatuses) = await runtime({ preconditionFailure("failed validation allowed a replacement") }, check: { _ in
            validationCalls += 1
            throw UserTunRuntime.stopFailed
        })
        requireStopFailure(await call(checkingRuntime, "checkConfig", ["config": "valid"]))
        requireStopFailure(await call(checkingRuntime, "checkConfig", ["config": "valid"]))
        requireStopFailure(await call(checkingRuntime, "start", startArguments))
        requireStopFailure(await call(checkingRuntime, "stop"))
        userTunRequire(validationCalls == 1 && checkingStatuses.states.last == "error")
    }

    private final class FakeNativeCloser {
        let entered = DispatchSemaphore(value: 0)
        let gate = DispatchSemaphore(value: 0)
        let block: Bool
        let fails: Bool
        private let lock = NSLock()
        private var calls = 0
        init(fails: Bool, block: Bool = false) { self.fails = fails; self.block = block }
        var count: Int { lock.lock(); defer { lock.unlock() }; return calls }
        func close(_ server: LibboxCommandServer) throws {
            lock.lock(); calls += 1; lock.unlock()
            // Construction/closing an empty standalone server opens no tunnel.
            // Keep the real native allocation drained while injecting failure.
            try server.closeWithError()
            entered.signal()
            if block { userTunRequire(gate.wait(timeout: .now() + 5) == .success) }
            if fails { throw NSError(domain: "private-native-close-credential", code: 1) }
        }
    }

    private static func serviceClose(_ service: UserTunService) async -> TunnelFailure? {
        await withCheckedContinuation { continuation in
            DispatchQueue.global().async {
                do { try service.close(); continuation.resume(returning: nil) }
                catch { continuation.resume(returning: error as? TunnelFailure ?? TunnelFailure.invalidConfig) }
            }
        }
    }

    private static func nativeCloseTests() async throws {
        for (dnsFails, nativeFails) in [(false, false), (true, false), (false, true), (true, true)] {
            let store = FakeDNSStore()
            let dns = UserTunDNSController { _, _, _ in store }
            try dns.install(servers: ["172.19.0.2"]) {}
            store.failRemove = dnsFails
            let native = FakeNativeCloser(fails: nativeFails)
            let service = try NativeUserTunService(dns: dns, closeNative: native.close)
            for _ in 0..<2 {
                let failure = await serviceClose(service)
                if dnsFails || nativeFails {
                    userTunRequire(failure?.code == UserTunRuntime.stopFailed.code && failure?.message == UserTunRuntime.stopFailed.message)
                } else { userTunRequire(failure == nil) }
            }
            userTunRequire(store.closeCount == 1 && native.count == 1, "one cleanup failure skipped or retried another subsystem")
            userTunRequire(store.value(forKey: store.addKeys[0]) == nil)
            do { _ = try service.prepare("private-config"); preconditionFailure("closed service prepared again") }
            catch { userTunRequire((error as? TunnelFailure)?.code == UserTunRuntime.cancelled.code) }
        }

        let native = FakeNativeCloser(fails: true, block: true)
        let service = try NativeUserTunService(closeNative: native.close)
        let first = Task { await serviceClose(service) }
        userTunRequire(await signalled(native.entered))
        let second = Task { await serviceClose(service) }
        native.gate.signal()
        let firstFailure = await first.value
        let secondFailure = await second.value
        userTunRequire(firstFailure?.code == UserTunRuntime.stopFailed.code && secondFailure?.code == UserTunRuntime.stopFailed.code)
        userTunRequire(firstFailure?.message == secondFailure?.message && native.count == 1)
    }

    private final class FakeChecker {
        let entered = DispatchSemaphore(value: 0)
        let prepareGate = DispatchSemaphore(value: 0)
        let cleanupEntered = DispatchSemaphore(value: 0)
        let cleanupGate = DispatchSemaphore(value: 0)
        private let lock = NSLock()
        private var calls = 0
        private var completed = 0
        var count: Int { lock.lock(); defer { lock.unlock() }; return calls }
        var running: Bool { lock.lock(); defer { lock.unlock() }; return calls != completed }
        func check(_ config: String) throws {
            lock.lock(); calls += 1; lock.unlock()
            entered.signal()
            userTunRequire(prepareGate.wait(timeout: .now() + 5) == .success)
            // The native checker owns its engine through deferred Close, too.
            defer {
                cleanupEntered.signal()
                userTunRequire(cleanupGate.wait(timeout: .now() + 5) == .success)
                lock.lock(); completed += 1; lock.unlock()
            }
            if config == "invalid" { throw NSError(domain: "private-config-diagnostic", code: 1) }
        }
    }

    private static func checkTests() async {
        let checker = FakeChecker()
        let service = FakeService()
        let (checkingRuntime, _) = await runtime({
            userTunRequire(!checker.running, "start constructed a service during validation")
            return service
        }, check: checker.check)
        let arguments = ["config": "invalid"]
        let pendingCheck = Task { await call(checkingRuntime, "checkConfig", arguments) }
        userTunRequire(await signalled(checker.entered))
        userTunRequire((await call(checkingRuntime, "getStatus") as? [String: Any])?["state"] as? String == "disconnected")
        userTunRequire((await call(checkingRuntime, "checkConfig", arguments) as? FlutterError)?.code == "busy")
        userTunRequire((await call(checkingRuntime, "start", startArguments) as? FlutterError)?.code == "busy")
        userTunRequire(await call(checkingRuntime, "stop") == nil)
        // A no-op stop must not release a validation reservation.
        userTunRequire((await call(checkingRuntime, "start", startArguments) as? FlutterError)?.code == "busy")
        checker.prepareGate.signal()
        userTunRequire(await signalled(checker.cleanupEntered))
        userTunRequire((await call(checkingRuntime, "checkConfig", arguments) as? FlutterError)?.code == "busy")
        userTunRequire((await call(checkingRuntime, "start", startArguments) as? FlutterError)?.code == "busy")
        userTunRequire(checker.count == 1)
        checker.cleanupGate.signal()
        let invalid = await pendingCheck.value as? FlutterError
        userTunRequire(invalid?.code == "invalid_config" && invalid?.message == TunnelFailure.invalidConfig.message)
        userTunRequire(await call(checkingRuntime, "start", startArguments) == nil)
        userTunRequire(await call(checkingRuntime, "stop") == nil)
        // Successful checks also release the reservation before their replies.
        for _ in 0..<2 {
            checker.prepareGate.signal()
            checker.cleanupGate.signal()
            userTunRequire(await call(checkingRuntime, "checkConfig", ["config": "valid"]) == nil)
        }
        userTunRequire(checker.count == 3)

        let services = [FakeService(blockPrepare: true), FakeService(blockActivate: true, blockClose: true)]
        let sessionChecker = FakeChecker()
        sessionChecker.prepareGate.signal()
        sessionChecker.cleanupGate.signal()
        var generation = 0
        let (sessionRuntime, statuses) = await runtime({
            defer { generation += 1 }
            return services[generation]
        }, check: sessionChecker.check)
        let preparing = Task { await call(sessionRuntime, "start", startArguments) }
        userTunRequire(await signalled(services[0].prepareEntered))
        userTunRequire((await call(sessionRuntime, "checkConfig", arguments) as? FlutterError)?.code == "busy")
        userTunRequire(await call(sessionRuntime, "stop") == nil)
        userTunRequire((await preparing.value as? FlutterError)?.code == "operation_cancelled")
        userTunRequire(await call(sessionRuntime, "start", startArguments) == nil)
        userTunRequire(await signalled(services[1].activateEntered))
        userTunRequire((await call(sessionRuntime, "checkConfig", arguments) as? FlutterError)?.code == "busy")
        services[1].activateGate.signal()
        userTunRequire(await eventually { statuses.states.last == "connected" })
        userTunRequire((await call(sessionRuntime, "checkConfig", arguments) as? FlutterError)?.code == "busy")
        let stopping = Task { await call(sessionRuntime, "stop") }
        userTunRequire(await signalled(services[1].closeEntered))
        userTunRequire((await call(sessionRuntime, "checkConfig", arguments) as? FlutterError)?.code == "busy")
        userTunRequire((await call(sessionRuntime, "getStatus") as? [String: Any])?["state"] as? String == "disconnecting")
        userTunRequire(sessionChecker.count == 0)
        services[1].closeGate.signal()
        userTunRequire(await stopping.value == nil)
        userTunRequire(await call(sessionRuntime, "checkConfig", ["config": "valid"]) == nil)
        userTunRequire(sessionChecker.count == 1)
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
        var beforeUpdate: ((Int32) -> Void)?
        func updateDefaultInterface(_ interfaceName: String?, interfaceIndex: Int32, isExpensive: Bool, isConstrained: Bool) {
            beforeUpdate?(interfaceIndex)
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

        // Hold a callback after Swift has selected an as-yet unknown utun.
        // Registration must return without waiting for that callback and must
        // queue the IPv6 physical refresh. The native assignment guard handles
        // the stale selected interface (covered by monitor_test.go).
        let racingSource = FakePathSource(ipv6)
        let racingMonitor = UserTunPathMonitor(factory: { racingSource }, initialTimeout: .seconds(3))
        let racingListener = PathListener()
        let selected = DispatchSemaphore(value: 0)
        let publishGate = DispatchSemaphore(value: 0)
        racingListener.beforeUpdate = { index in
            if index == 600 {
                selected.signal()
                userTunRequire(publishGate.wait(timeout: .now() + 5) == .success)
            }
        }
        let starting = Task {
            await withCheckedContinuation { continuation in
                DispatchQueue.global().async {
                    do { try racingMonitor.startDefaultInterfaceMonitor(racingListener); continuation.resume(returning: true) }
                    catch { continuation.resume(returning: false) }
                }
            }
        }
        userTunRequire(await signalled(selected))
        let registered = DispatchSemaphore(value: 0)
        DispatchQueue.global().async {
            racingMonitor.registerMyInterface("own-dynamic-tunnel")
            registered.signal()
        }
        userTunRequire(await signalled(registered), "ownership registration waited on a listener callback")
        publishGate.signal()
        userTunRequire(await starting.value)
        userTunRequire(await eventually { racingListener.updates == [600, 601] })
        try racingMonitor.closeDefaultInterfaceMonitor(racingListener)
    }
}
