// SPDX-License-Identifier: GPL-3.0-or-later
import Darwin
import FlutterMacOS
import Foundation

/// Serializes state only. Potentially blocking constructors, activation and
/// permanent cancellation run on independent workers so stop/status stay usable.
final class UserTunRuntime {
    struct Dependencies {
        var version: () throws -> String
        var setup: () throws -> Void
        var check: (String) throws -> Void
        var makeService: () throws -> UserTunService
        var isPrivileged: () -> Bool

        static let live = Dependencies(
            version: {
                let capabilities = try CoreCapabilities.load()
                try NativeCore.verify(capabilities)
                return capabilities.version
            },
            setup: {
                let capabilities = try CoreCapabilities.load()
                try NativeCore.verify(capabilities)
                do { try NativeCore.setup(directory: UserTunRuntime.runtimeDirectory(), crashReportSource: "macOS user_tun") }
                catch let failure as TunnelFailure { throw failure }
                catch { throw TunnelFailure(code: "storage_error", message: "The private macOS core directories could not be accessed.") }
            },
            check: NativeUserTunService.check,
            makeService: { try NativeUserTunService() },
            isPrivileged: { geteuid() == 0 }
        )
    }

    private final class Session {
        enum Phase { case preparing, activating, connected, stopping }
        let service: UserTunService
        var phase = Phase.preparing
        var workerFinished = false
        var closeFinished = false
        var finalStatus: TunnelStatus?
        var finalStatusEmitted = false
        var startReply: FlutterResult?
        var stopReplies: [FlutterResult] = []
        init(service: UserTunService, reply: @escaping FlutterResult) { self.service = service; startReply = reply }
    }

    static let shared = UserTunRuntime(dependencies: .live)
    private let queue = DispatchQueue(label: "dev.idy.sing_box_flutter.user-tun.state")
    private let workers = DispatchQueue(label: "dev.idy.sing_box_flutter.user-tun.work", attributes: .concurrent)
    private let dependencies: Dependencies
    private var initialized = false
    private var session: Session?
    private var checking = false
    private var status = TunnelStatus("disconnected")
    private var observers: [UUID: (TunnelStatus) -> Void] = [:]

    init(dependencies: Dependencies) { self.dependencies = dependencies }

    func addObserver(_ observer: @escaping (TunnelStatus) -> Void) -> UUID {
        let token = UUID()
        queue.async {
            self.observers[token] = observer
            let current = self.status
            DispatchQueue.main.async { observer(current) }
        }
        return token
    }

    func removeObserver(_ token: UUID) { queue.async { self.observers.removeValue(forKey: token) } }

    func handle(_ call: FlutterMethodCall, result: @escaping FlutterResult) {
        queue.async {
            do {
                let arguments = call.arguments as? [String: Any] ?? [:]
                if call.method == "getVersion" { self.reply(result, try self.dependencies.version()); return }
                if call.method == "initialize" {
                    if !self.initialized { try self.dependencies.setup(); self.initialized = true }
                    self.reply(result)
                    return
                }
                guard ["checkConfig", "requestPermission", "start", "stop", "getStatus"].contains(call.method) else {
                    self.reply(result, FlutterMethodNotImplemented)
                    return
                }
                guard self.initialized else { throw TunnelFailure.notInitialized }
                switch call.method {
                case "getStatus": self.reply(result, self.status.event)
                case "requestPermission": self.reply(result, self.dependencies.isPrivileged())
                case "checkConfig":
                    guard let config = arguments["config"] as? String, !config.isEmpty else { throw TunnelFailure.invalidArguments }
                    try self.check(config, reply: result)
                case "start": try self.start(arguments, reply: result)
                case "stop": self.stop(reply: result)
                default: break
                }
            } catch { self.reply(result, failure: error as? TunnelFailure ?? .invalidConfig) }
        }
    }

    private func check(_ config: String, reply result: @escaping FlutterResult) throws {
        // The standalone checker constructs and closes an engine using the same
        // native setup directories. Reserve it until cleanup has returned, just
        // as a session owns those resources through both teardown workers.
        guard session == nil, !checking else { throw TunnelFailure.busy }
        checking = true
        workers.async {
            var failure: TunnelFailure?
            do { try self.dependencies.check(config) }
            catch { failure = error as? TunnelFailure ?? .invalidConfig }
            let outcome = failure
            self.queue.async {
                self.checking = false
                self.reply(result, failure: outcome)
            }
        }
    }

    private func start(_ arguments: [String: Any], reply result: @escaping FlutterResult) throws {
        // Permission takes precedence over malformed config, arguments and busy.
        guard dependencies.isPrivileged() else { throw Self.permissionRequired }
        guard let config = arguments["config"] as? String, !config.isEmpty,
              let name = arguments["name"] as? String, !name.isEmpty else { throw TunnelFailure.invalidArguments }
        guard session == nil, !checking else { throw TunnelFailure.busy }
        let current = Session(service: try dependencies.makeService(), reply: result)
        session = current // Establish cancellation ownership before entering Go.
        workers.async {
            let outcome: Result<UserTunDNSPlan, Error>
            do { outcome = .success(try current.service.prepare(config)) }
            catch { outcome = .failure(error) }
            self.queue.async { self.prepared(current, outcome: outcome) }
        }
    }

    private func prepared(_ current: Session, outcome: Result<UserTunDNSPlan, Error>) {
        guard session === current else { return }
        guard current.phase == .preparing else {
            current.workerFinished = true
            finishTeardown(current)
            return
        }
        switch outcome {
        case .failure(let error):
            current.workerFinished = true
            finishStart(current, failure: error as? TunnelFailure ?? .invalidConfig)
            beginTeardown(current, finalStatus: nil)
        case .success(let plan):
            guard plan.hasTun else {
                current.workerFinished = true
                finishStart(current, failure: Self.missingTun)
                beginTeardown(current, finalStatus: nil)
                return
            }
            current.phase = .activating
            emit(TunnelStatus("connecting"))
            // A successful Future acknowledges acceptance. Network startup may
            // block; the UI must already be able to request stop while connecting.
            finishStart(current)
            workers.async { [self, current] in
                var failure: TunnelFailure?
                do {
                    try current.service.activate { [weak self, weak current] in
                        guard let self, let current else { return }
                        self.queue.async { self.dnsFailed(current) }
                    }
                } catch { failure = error as? TunnelFailure ?? Self.startFailed }
                let outcome = failure
                self.queue.async { self.activated(current, failure: outcome) }
            }
        }
    }

    private func activated(_ current: Session, failure: TunnelFailure?) {
        guard session === current else { return }
        current.workerFinished = true
        guard current.phase == .activating else { finishTeardown(current); return }
        if let failure {
            beginTeardown(current, finalStatus: TunnelStatus("error", message: failure.message))
        } else {
            current.phase = .connected
            emit(TunnelStatus("connected"))
        }
    }

    private func dnsFailed(_ current: Session) {
        guard session === current, current.phase != .stopping else { return }
        beginTeardown(current, finalStatus: TunnelStatus("error", message: UserTunDNSController.configurationFailure.message))
    }

    private func stop(reply result: @escaping FlutterResult) {
        guard let current = session else {
            emit(TunnelStatus("disconnected"))
            reply(result)
            return
        }
        current.stopReplies.append(result)
        finishStart(current, failure: Self.cancelled)
        emit(TunnelStatus("disconnecting"))
        beginTeardown(current, finalStatus: TunnelStatus("disconnected"))
    }

    private func beginTeardown(_ current: Session, finalStatus: TunnelStatus?) {
        let alreadyStopping = current.phase == .stopping
        current.phase = .stopping
        current.finalStatus = finalStatus
        current.finalStatusEmitted = false
        if let finalStatus, finalStatus.state == "error" {
            // DNS loss must stop reporting connected even if native teardown
            // still has workers to drain. The session remains busy until done.
            emit(finalStatus)
            current.finalStatusEmitted = true
        }
        guard !alreadyStopping else { finishTeardown(current); return }
        // This worker must run concurrently with the startup worker. Go Close
        // marks closed/cancels before acquiring its operation mutex.
        workers.async {
            var failed = false
            do { try current.service.close() } catch { failed = true }
            let closeFailed = failed
            self.queue.async {
                guard self.session === current else { return }
                current.closeFinished = true
                if closeFailed {
                    current.finalStatus = TunnelStatus("error", message: "The macOS user_tun service could not be stopped cleanly.")
                    current.finalStatusEmitted = false
                }
                self.finishTeardown(current)
            }
        }
    }

    private func finishTeardown(_ current: Session) {
        guard session === current, current.closeFinished, current.workerFinished else { return }
        session = nil // A replacement cannot start until both workers are done.
        if let finalStatus = current.finalStatus, !current.finalStatusEmitted { emit(finalStatus) }
        for result in current.stopReplies {
            if current.finalStatus?.state == "error" {
                reply(result, failure: TunnelFailure(code: "core_stop_failed", message: "The macOS user_tun service could not be stopped cleanly."))
            } else { reply(result) }
        }
        current.stopReplies.removeAll()
    }

    private func finishStart(_ current: Session, failure: TunnelFailure? = nil) {
        guard let result = current.startReply else { return }
        current.startReply = nil
        reply(result, failure: failure)
    }

    private func reply(_ result: @escaping FlutterResult, _ value: Any? = nil, failure: TunnelFailure? = nil) {
        DispatchQueue.main.async {
            if let failure { result(FlutterError(code: failure.code, message: failure.message, details: nil)) }
            else { result(value) }
        }
    }

    private func emit(_ status: TunnelStatus) {
        self.status = status
        let listeners = Array(observers.values)
        DispatchQueue.main.async { listeners.forEach { $0(status) } }
    }

    static let missingTun = TunnelFailure(code: "missing_tun", message: "The configuration must start a TUN inbound in the macOS user_tun backend.")
    static let cancelled = TunnelFailure(code: "operation_cancelled", message: "The macOS user_tun startup was stopped.")
    static let startFailed = TunnelFailure(code: "core_start_failed", message: "sing-box could not start macOS user_tun. Check the configuration and run the host as root without App Sandbox.")
    private static let permissionRequired = TunnelFailure(code: "permission_required", message: "macOS user_tun requires an already elevated, unsandboxed process running as root. This plugin does not elevate privileges.")

    private static func runtimeDirectory() throws -> URL {
        let support = try FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
        let directory = support.appendingPathComponent(Bundle.main.bundleIdentifier ?? "dev.idy.sing_box_flutter", isDirectory: true)
            .appendingPathComponent("SingBoxFlutter/Runtime", isDirectory: true)
        for path in [directory, directory.appendingPathComponent("Working"), directory.appendingPathComponent("Temp")] {
            try FileManager.default.createDirectory(at: path, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: path.path)
        }
        var resourceURL = directory
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        try resourceURL.setResourceValues(values)
        return directory
    }
}
