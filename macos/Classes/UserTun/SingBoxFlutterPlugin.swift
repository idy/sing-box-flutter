// SPDX-License-Identifier: GPL-3.0-or-later
import FlutterMacOS
import Foundation

/// Channels attach to the process-owned service. Cancelling events or detaching
/// a Flutter engine never closes the standalone core or changes its configuration.
public final class SingBoxFlutterPlugin: NSObject, FlutterPlugin, FlutterStreamHandler {
    private var eventSink: FlutterEventSink?
    private var observer: UUID?

    public static func register(with registrar: FlutterPluginRegistrar) {
        let instance = SingBoxFlutterPlugin()
        let methods = FlutterMethodChannel(name: "dev.idy.sing_box_flutter/methods", binaryMessenger: registrar.messenger)
        let events = FlutterEventChannel(name: "dev.idy.sing_box_flutter/events", binaryMessenger: registrar.messenger)
        registrar.addMethodCallDelegate(instance, channel: methods)
        events.setStreamHandler(instance)
    }

    deinit {
        if let observer { UserTunRuntime.shared.removeObserver(observer) }
    }

    public func detachFromEngine(for registrar: FlutterPluginRegistrar) {
        eventSink = nil
        if let observer { UserTunRuntime.shared.removeObserver(observer) }
        observer = nil
    }

    public func handle(_ call: FlutterMethodCall, result: @escaping FlutterResult) {
        UserTunRuntime.shared.handle(call, result: result)
    }

    public func onListen(withArguments arguments: Any?, eventSink events: @escaping FlutterEventSink) -> FlutterError? {
        eventSink = events
        if let observer { UserTunRuntime.shared.removeObserver(observer) }
        observer = UserTunRuntime.shared.addObserver { [weak self] status in self?.eventSink?(status.event) }
        return nil
    }

    public func onCancel(withArguments arguments: Any?) -> FlutterError? {
        eventSink = nil
        if let observer { UserTunRuntime.shared.removeObserver(observer) }
        observer = nil
        return nil
    }
}
