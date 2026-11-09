// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import Libbox

struct TunnelFailure: LocalizedError {
    let code: String
    let message: String
    var errorDescription: String? { message }

    static let notInitialized = TunnelFailure(code: "not_initialized", message: "Call initialize before using this method.")
    static let invalidArguments = TunnelFailure(code: "invalid_arguments", message: "Required arguments are missing or invalid.")
    static let invalidConfig = TunnelFailure(code: "invalid_config", message: "libbox rejected the configuration. Check it locally without sharing credentials.")
    static let busy = TunnelFailure(code: "busy", message: "Stop the current tunnel before starting or changing its configuration.")
    static let storage = TunnelFailure(code: "storage_error", message: "The App Group configuration or state could not be accessed.")
    static let unsupported = TunnelFailure(code: "unsupported", message: "This feature is not supported by the iOS backend.")
}

struct TunnelStatus: Codable {
    let state: String
    let message: String?
    let sessionID: String?

    init(_ state: String, message: String? = nil, sessionID: String? = nil) {
        self.state = state
        self.message = message.map { String($0.prefix(240)) }
        self.sessionID = sessionID
    }

    var event: [String: Any] {
        var value: [String: Any] = ["type": "status", "state": state]
        if let message { value["message"] = message }
        return value
    }
}

struct TunnelConfiguration: Codable {
    let version: Int
    let sessionID: String
    let config: String
    let name: String
}

/// Shared files contain credentials. Neither configuration nor upstream errors are sent over events.
final class TunnelStorage {
    let appGroup: String
    let root: URL
    private var configurationURL: URL { root.appendingPathComponent("configuration.json") }
    private var statusURL: URL { root.appendingPathComponent("status.json") }

    init(appGroup: String) throws {
        guard appGroup.hasPrefix("group."),
              let container = FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: appGroup) else {
            throw TunnelFailure(code: "app_group_unavailable", message: "The App Group container is unavailable. Check both targets' entitlements and signing.")
        }
        self.appGroup = appGroup
        root = container.appendingPathComponent("SingBoxFlutter", isDirectory: true)
        try createDirectory(root)
        var resourceURL = root
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        try resourceURL.setResourceValues(values)
    }

    func runtimeDirectory(validation: Bool) throws -> URL {
        let directory = root.appendingPathComponent(validation ? "Validation" : "Runtime", isDirectory: true)
        try createDirectory(directory)
        try createDirectory(directory.appendingPathComponent("Working", isDirectory: true))
        try createDirectory(directory.appendingPathComponent("Temp", isDirectory: true))
        return directory
    }

    func saveConfiguration(_ config: String, name: String) throws -> TunnelConfiguration {
        let value = TunnelConfiguration(version: 1, sessionID: UUID().uuidString, config: config, name: name)
        try write(value, to: configurationURL)
        return value
    }

    func loadConfiguration() throws -> TunnelConfiguration {
        let value = try JSONDecoder().decode(TunnelConfiguration.self, from: Data(contentsOf: configurationURL))
        guard value.version == 1, !value.sessionID.isEmpty, !value.config.isEmpty else { throw TunnelFailure.storage }
        return value
    }

    func writeStatus(_ status: TunnelStatus) throws { try write(status, to: statusURL) }

    func readStatus() -> TunnelStatus? {
        guard let data = try? Data(contentsOf: statusURL),
              let value = try? JSONDecoder().decode(TunnelStatus.self, from: data),
              ["disconnected", "connecting", "connected", "disconnecting", "error"].contains(value.state) else { return nil }
        return value
    }

    private func createDirectory(_ url: URL) throws {
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true, attributes: [
            .posixPermissions: 0o700,
            .protectionKey: FileProtectionType.completeUntilFirstUserAuthentication,
        ])
    }

    private func write<T: Encodable>(_ value: T, to url: URL) throws {
        try JSONEncoder().encode(value).write(to: url, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }
}

enum NativeCore {
    static func setup(storage: TunnelStorage, validation: Bool) throws {
        let directory = try storage.runtimeDirectory(validation: validation)
        let options = LibboxSetupOptions()
        options.basePath = directory.path
        options.workingPath = directory.appendingPathComponent("Working").path
        options.tempPath = directory.appendingPathComponent("Temp").path
        options.logMaxLines = 100
        options.debug = false
        options.crashReportSource = validation ? "Validation" : "PacketTunnel"
        options.commandServerSecret = UUID().uuidString
        options.oomKillerEnabled = !validation
        var error: NSError?
        LibboxSetup(options, &error)
        if error != nil {
            throw TunnelFailure(code: "core_setup_failed", message: "libbox could not initialize its private directories.")
        }
    }

    static func check(_ config: String) throws {
        var error: NSError?
        LibboxCheckConfig(config, &error)
        // Deliberately discard upstream error text: it can include JSON and credentials.
        if error != nil { throw TunnelFailure.invalidConfig }
    }
}
