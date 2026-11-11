// SPDX-License-Identifier: GPL-3.0-or-later
#if os(macOS)
import FlutterMacOS
#else
import Flutter
#endif
import Foundation
import NetworkExtension

/// Owns system preferences and Flutter channels, never the running VPN service.
#if SINGBOX_SWIFT_PACKAGE
@objc(SBFSelectedController)
#endif
public final class SingBoxFlutterPlugin: NSObject, FlutterPlugin, FlutterStreamHandler {
    private let validationQueue = DispatchQueue(label: "dev.idy.sing_box_flutter.validation")
    private var commands: [() -> Void] = []
    private var executing = false
    private var storage: TunnelStorage?
    private var providerBundleIdentifier: String?
    private var notificationTitle = "sing-box"
    private var manager: NETunnelProviderManager?
    private var statusObserver: NSObjectProtocol?
    private var eventSink: FlutterEventSink?
    private var startRequested = false
    private var stopRequested = false
    private var status = TunnelStatus("disconnected")

    public static func register(with registrar: FlutterPluginRegistrar) {
        let instance = SingBoxFlutterPlugin()
        #if os(macOS)
        let messenger = registrar.messenger
        #else
        let messenger = registrar.messenger()
        #endif
        let methods = FlutterMethodChannel(name: "dev.idy.sing_box_flutter/methods", binaryMessenger: messenger)
        let events = FlutterEventChannel(name: "dev.idy.sing_box_flutter/events", binaryMessenger: messenger)
        registrar.addMethodCallDelegate(instance, channel: methods)
        events.setStreamHandler(instance)
    }

    deinit {
        if let statusObserver { NotificationCenter.default.removeObserver(statusObserver) }
    }

    public func detachFromEngine(for registrar: FlutterPluginRegistrar) {
        eventSink = nil
        // The OS-owned tunnel deliberately survives engine teardown.
    }

    public func handle(_ call: FlutterMethodCall, result: @escaping FlutterResult) {
        // All manager state and Flutter results are confined to the main queue.
        DispatchQueue.main.async { self.enqueue { self.execute(call, result: result) } }
    }

    private func enqueue(_ command: @escaping () -> Void) {
        commands.append(command)
        runNext()
    }

    private func runNext() {
        guard !executing, !commands.isEmpty else { return }
        executing = true
        commands.removeFirst()()
    }

    private func finish(_ result: @escaping FlutterResult, _ value: Any? = nil) {
        result(value)
        executing = false
        runNext()
    }

    private func fail(_ result: @escaping FlutterResult, _ failure: TunnelFailure) {
        finish(result, FlutterError(code: failure.code, message: failure.message, details: nil))
    }

    private func execute(_ call: FlutterMethodCall, result: @escaping FlutterResult) {
        let arguments = call.arguments as? [String: Any] ?? [:]
        if call.method == "getVersion" {
            do { finish(result, try CoreCapabilities.load().version) }
            catch { fail(result, .coreMismatch) }
            return
        }
        if call.method == "initialize" {
            initialize(arguments, result: result)
            return
        }
        guard ["checkConfig", "requestPermission", "start", "stop", "getStatus"].contains(call.method) else {
            finish(result, FlutterMethodNotImplemented)
            return
        }
        guard let storage else { fail(result, .notInitialized); return }
        switch call.method {
        case "checkConfig":
            guard let config = arguments["config"] as? String, !config.isEmpty else { fail(result, .invalidArguments); return }
            validationQueue.async {
                do {
                    try ConfigPreflight.check(config, capabilities: CoreCapabilities.load())
                    DispatchQueue.main.async { self.finish(result) }
                } catch let failure as TunnelFailure {
                    // Only fixed, credential-free codes/messages are ever constructed.
                    DispatchQueue.main.async { self.fail(result, failure) }
                } catch {
                    DispatchQueue.main.async { self.fail(result, .invalidConfig) }
                }
            }
        case "requestPermission":
            requestPermission(result: result)
        case "start":
            guard let config = arguments["config"] as? String, !config.isEmpty,
                  let name = arguments["name"] as? String, !name.isEmpty else { fail(result, .invalidArguments); return }
            guard !isActive else { fail(result, .busy); return }
            guard let manager, manager.isEnabled, let session = manager.connection as? NETunnelProviderSession else {
                fail(result, TunnelFailure(code: "permission_required", message: "Call requestPermission and allow the VPN configuration before starting."))
                return
            }
            validationQueue.async {
                do {
                    let capabilities = try CoreCapabilities.load()
                    try ConfigPreflight.check(config, capabilities: capabilities, requireTUN: true)
                    DispatchQueue.main.async {
                        // The OS can start the saved VPN from Settings while validation runs.
                        guard !self.isActive else { self.fail(result, .busy); return }
                        let configuration: TunnelConfiguration
                        do { configuration = try storage.saveConfiguration(config, name: name, coreFingerprint: capabilities.fingerprint) }
                        catch { self.fail(result, .storage); return }
                        do {
                            self.startRequested = true
                            try session.startVPNTunnel(options: ["sessionID": configuration.sessionID as NSString])
                            self.emit(TunnelStatus("connecting"))
                            self.finish(result)
                        } catch {
                            self.startRequested = false
                            try? storage.writeStatus(TunnelStatus("error", message: "The system rejected the VPN start request.", sessionID: configuration.sessionID))
                            self.emit(TunnelStatus("error", message: "The system rejected the VPN start request."))
                            self.fail(result, TunnelFailure(code: "start_failed", message: "The system rejected the VPN start request. Check the extension target and VPN permission."))
                        }
                    }
                } catch let failure as TunnelFailure {
                    DispatchQueue.main.async { self.fail(result, failure) }
                } catch {
                    DispatchQueue.main.async { self.fail(result, .storage) }
                }
            }
        case "stop":
            let needsStop = isActive
            startRequested = false
            if needsStop {
                stopRequested = true
                manager?.connection.stopVPNTunnel()
                emit(TunnelStatus("disconnecting"))
            } else {
                // Clear a previous failed attempt only once the system is already stopped.
                try? storage.writeStatus(TunnelStatus("disconnected"))
                emit(TunnelStatus("disconnected"))
            }
            finish(result)
        case "getStatus":
            refreshStatus()
            queryProviderStatus(result: result)
        default:
            finish(result, FlutterMethodNotImplemented)
        }
    }

    private var isActive: Bool {
        guard let state = manager?.connection.status else { return startRequested || stopRequested }
        return startRequested || stopRequested || ![NEVPNStatus.invalid, .disconnected].contains(state)
    }

    private func initialize(_ arguments: [String: Any], result: @escaping FlutterResult) {
        guard let group = arguments["appGroup"] as? String, !group.isEmpty,
              let bundle = arguments["providerBundleIdentifier"] as? String, !bundle.isEmpty,
              let title = arguments["notificationTitle"] as? String, !title.isEmpty else { fail(result, .invalidArguments); return }
        if let storage, storage.appGroup == group, providerBundleIdentifier == bundle {
            notificationTitle = title
            refreshStatus()
            finish(result)
            return
        }
        guard !isActive else { fail(result, .busy); return }
        do {
            let newStorage = try TunnelStorage(appGroup: group)
            NETunnelProviderManager.loadAllFromPreferences { managers, error in
                DispatchQueue.main.async {
                    guard error == nil else {
                        self.fail(result, TunnelFailure(code: "preferences_failed", message: "VPN preferences could not be loaded. Check the host app's signing and capabilities."))
                        return
                    }
                    self.validationQueue.async {
                        do {
                            _ = try CoreCapabilities.load()
                            DispatchQueue.main.async {
                                self.storage = newStorage
                                self.providerBundleIdentifier = bundle
                                self.notificationTitle = title
                                self.setManager(self.findManager(managers, bundle: bundle, appGroup: group))
                                self.refreshStatus()
                                self.finish(result)
                            }
                        } catch let failure as TunnelFailure {
                            DispatchQueue.main.async { self.fail(result, failure) }
                        } catch {
                            DispatchQueue.main.async { self.fail(result, .storage) }
                        }
                    }
                }
            }
        } catch let failure as TunnelFailure {
            fail(result, failure)
        } catch {
            fail(result, .storage)
        }
    }

    private func findManager(_ managers: [NETunnelProviderManager]?, bundle: String, appGroup: String) -> NETunnelProviderManager? {
        managers?.first {
            guard let tunnel = $0.protocolConfiguration as? NETunnelProviderProtocol else { return false }
            return tunnel.providerBundleIdentifier == bundle && tunnel.providerConfiguration?["appGroup"] as? String == appGroup
        }
    }

    private func requestPermission(result: @escaping FlutterResult) {
        guard let storage, let bundle = providerBundleIdentifier else { fail(result, .notInitialized); return }
        NETunnelProviderManager.loadAllFromPreferences { managers, error in
            DispatchQueue.main.async {
                guard error == nil else {
                    self.fail(result, TunnelFailure(code: "preferences_failed", message: "VPN preferences could not be loaded."))
                    return
                }
                let manager = self.findManager(managers, bundle: bundle, appGroup: storage.appGroup) ?? NETunnelProviderManager()
                self.setManager(manager)
                if manager.isEnabled, manager.protocolConfiguration != nil {
                    self.refreshStatus()
                    self.finish(result, true)
                    return
                }
                guard !self.isActive else { self.fail(result, .busy); return }
                let tunnel = NETunnelProviderProtocol()
                tunnel.providerBundleIdentifier = bundle
                tunnel.providerConfiguration = ["appGroup": storage.appGroup, "schemaVersion": 1]
                tunnel.serverAddress = "sing-box"
                tunnel.disconnectOnSleep = false
                manager.protocolConfiguration = tunnel
                manager.localizedDescription = self.notificationTitle
                manager.isEnabled = true
                manager.saveToPreferences { error in
                    DispatchQueue.main.async {
                        if let error {
                            self.setManager(nil)
                            if self.isConsentCancellation(error as NSError) { self.finish(result, false) }
                            else { self.fail(result, TunnelFailure(code: "permission_failed", message: "The VPN configuration could not be saved. Check both targets' signing and Network Extension entitlements.")) }
                            return
                        }
                        // Apple's newly saved preferences must be reloaded before startVPNTunnel.
                        manager.loadFromPreferences { error in
                            DispatchQueue.main.async {
                                guard error == nil else {
                                    self.setManager(nil)
                                    self.fail(result, TunnelFailure(code: "preferences_failed", message: "The saved VPN configuration could not be reloaded."))
                                    return
                                }
                                self.setManager(manager)
                                self.refreshStatus()
                                self.finish(result, manager.isEnabled)
                            }
                        }
                    }
                }
            }
        }
    }

    private func isConsentCancellation(_ error: NSError) -> Bool {
        if error.domain == NSCocoaErrorDomain && error.code == NSUserCancelledError { return true }
        // NEConfiguration reports denied installation, including dismissal of system consent, as code 10.
        if error.domain == "NEConfigurationErrorDomain" && error.code == 10 { return true }
        if let underlying = error.userInfo[NSUnderlyingErrorKey] as? NSError { return isConsentCancellation(underlying) }
        return false
    }

    private func setManager(_ manager: NETunnelProviderManager?) {
        if let statusObserver { NotificationCenter.default.removeObserver(statusObserver) }
        self.manager = manager
        guard let manager else { statusObserver = nil; return }
        statusObserver = NotificationCenter.default.addObserver(forName: .NEVPNStatusDidChange, object: manager.connection, queue: .main) { [weak self] _ in
            guard let self else { return }
            self.startRequested = false
            if [.disconnected, .invalid].contains(manager.connection.status) {
                self.stopRequested = false
            } else if self.stopRequested, [.connecting, .connected, .reasserting].contains(manager.connection.status) {
                manager.connection.stopVPNTunnel()
            }
            self.refreshStatus()
        }
    }

    private func refreshStatus() {
        if stopRequested { emit(TunnelStatus("disconnecting")); return }
        let state = manager?.connection.status ?? .disconnected
        switch state {
        case .connecting: emit(TunnelStatus("connecting"))
        case .connected: emit(TunnelStatus("connected"))
        case .reasserting: emit(TunnelStatus("connecting"))
        case .disconnecting: emit(TunnelStatus("disconnecting"))
        case .invalid, .disconnected:
            if startRequested { emit(TunnelStatus("connecting")); return }
            if let saved = storage?.readStatus(), saved.state == "error",
               let currentSessionID = storage?.currentSessionID(), saved.sessionID == currentSessionID {
                emit(TunnelStatus("error", message: saved.message))
            } else { emit(TunnelStatus("disconnected")) }
        @unknown default: emit(TunnelStatus("error", message: "The system returned an unknown VPN status."))
        }
    }

    private func queryProviderStatus(result: @escaping FlutterResult) {
        guard !stopRequested, let manager, [.connected, .reasserting].contains(manager.connection.status),
              let session = manager.connection as? NETunnelProviderSession,
              let request = try? JSONSerialization.data(withJSONObject: ["version": 1, "command": "getStatus"]) else {
            finish(result, status.event)
            return
        }
        var completed = false
        let complete: (Data?) -> Void = { data in
            guard !completed else { return }
            completed = true
            // Do not let a delayed provider reply override a more recent OS disconnect.
            self.refreshStatus()
            if manager.connection.status == .connected,
               let data, let reply = try? JSONDecoder().decode(TunnelStatus.self, from: data),
               ["connected", "error"].contains(reply.state) {
                self.emit(TunnelStatus(reply.state, message: reply.message))
            }
            self.finish(result, self.status.event)
        }
        do {
            try session.sendProviderMessage(request) { data in DispatchQueue.main.async { complete(data) } }
            DispatchQueue.main.asyncAfter(deadline: .now() + 2) { complete(nil) }
        } catch { complete(nil) }
    }

    private func emit(_ value: TunnelStatus) {
        status = value
        eventSink?(value.event)
    }

    public func onListen(withArguments arguments: Any?, eventSink events: @escaping FlutterEventSink) -> FlutterError? {
        eventSink = events
        refreshStatus()
        return nil
    }

    public func onCancel(withArguments arguments: Any?) -> FlutterError? {
        eventSink = nil
        return nil
    }
}
