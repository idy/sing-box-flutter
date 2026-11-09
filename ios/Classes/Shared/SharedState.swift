// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation

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

    func runtimeDirectory() throws -> URL {
        let directory = root.appendingPathComponent("Runtime", isDirectory: true)
        try createDirectory(directory)
        try createDirectory(directory.appendingPathComponent("Working", isDirectory: true))
        try createDirectory(directory.appendingPathComponent("Temp", isDirectory: true))
        return directory
    }

    func saveConfiguration(_ config: String, name: String, coreFingerprint: String) throws -> TunnelConfiguration {
        let value = TunnelConfiguration(version: 1, sessionID: UUID().uuidString, config: config, name: name, coreFingerprint: coreFingerprint)
        try write(value, to: configurationURL)
        try write(SessionIdentity(sessionID: value.sessionID), to: root.appendingPathComponent("session.json"))
        return value
    }

    private struct SessionIdentity: Codable { let sessionID: String }

    func currentSessionID() -> String? {
        guard let data = try? Data(contentsOf: root.appendingPathComponent("session.json")) else { return nil }
        return (try? JSONDecoder().decode(SessionIdentity.self, from: data))?.sessionID
    }

    func loadConfiguration() throws -> TunnelConfiguration {
        let value = try JSONDecoder().decode(TunnelConfiguration.self, from: Data(contentsOf: configurationURL))
        guard value.version == 1, !value.sessionID.isEmpty, !value.config.isEmpty else { throw TunnelFailure.storage }
        // Also migrate old saved sessions when the extension next starts them.
        try write(SessionIdentity(sessionID: value.sessionID), to: root.appendingPathComponent("session.json"))
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
