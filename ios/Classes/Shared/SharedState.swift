// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation

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

struct StoredTunnelConfiguration {
    let configuration: TunnelConfiguration
    let reference: Data
}

/// App Group files hold status and libbox working directories. Full configuration
/// payloads (for every endpoint type) are stored only in the shared device-only Keychain.
final class TunnelStorage {
    let identity: ConfigurationIdentity
    var appGroup: String { identity.appGroup }
    var keychainAccessGroup: String { identity.accessGroup }
    let root: URL
    private let keychain: KeychainConfigurationStore
    private var legacyConfigurationURL: URL { root.appendingPathComponent("configuration.json") }
    private var statusURL: URL { root.appendingPathComponent("status.json") }

    init(identity: ConfigurationIdentity, container: URL? = nil, keychainClient: ConfigurationKeychainClient = SystemConfigurationKeychainClient()) throws {
        guard let container = container ?? FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: identity.appGroup) else {
            throw TunnelFailure(code: "app_group_unavailable", message: "The App Group container is unavailable. Check both targets' entitlements and signing.")
        }
        self.identity = identity
        keychain = KeychainConfigurationStore(identity: identity, client: keychainClient)
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

    func saveConfiguration(_ config: String, name: String) throws -> StoredTunnelConfiguration {
        let value = TunnelConfiguration(version: 2, sessionID: UUID().uuidString, config: config, name: name)
        let reference = try keychain.put(JSONEncoder().encode(value))
        return StoredTunnelConfiguration(configuration: value, reference: reference)
    }

    func loadConfiguration(reference: Data, expectedSessionID: String? = nil) throws -> TunnelConfiguration {
        let data = try keychain.read(reference: reference)
        let value = try decodeConfiguration(data, version: 2)
        if let expectedSessionID, value.sessionID != expectedSessionID {
            throw TunnelFailure(code: "stale_configuration", message: "Save the configuration again from the app before starting the VPN.")
        }
        return value
    }

    /// Host-only, one-time upgrade of pre-Keychain sing-box profiles. The provider
    /// never calls this or falls back to this file when a reference cannot be read.
    /// Delete the file only after the secure reference is saved in VPN preferences.
    func legacyConfigurationForMigration() throws -> TunnelConfiguration? {
        guard FileManager.default.fileExists(atPath: legacyConfigurationURL.path) else { return nil }
        return try decodeConfiguration(Data(contentsOf: legacyConfigurationURL), version: 1)
    }

    func removeLegacyConfiguration() throws {
        if FileManager.default.fileExists(atPath: legacyConfigurationURL.path) {
            try FileManager.default.removeItem(at: legacyConfigurationURL)
        }
    }

    private func decodeConfiguration(_ data: Data, version: Int) throws -> TunnelConfiguration {
        guard data.count <= ConfigurationIdentity.maximumConfigurationBytes,
              let value = try? JSONDecoder().decode(TunnelConfiguration.self, from: data),
              value.version == version, UUID(uuidString: value.sessionID) != nil,
              !value.config.isEmpty, !value.name.isEmpty else { throw TunnelFailure.storage }
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
        var attributes: [FileAttributeKey: Any] = [.posixPermissions: 0o700]
        #if os(iOS)
        attributes[.protectionKey] = FileProtectionType.completeUntilFirstUserAuthentication
        #endif
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true, attributes: attributes)
    }

    private func write<T: Encodable>(_ value: T, to url: URL) throws {
        var options: Data.WritingOptions = [.atomic]
        #if os(iOS)
        options.insert(.completeFileProtectionUntilFirstUserAuthentication)
        #endif
        try JSONEncoder().encode(value).write(to: url, options: options)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }
}
