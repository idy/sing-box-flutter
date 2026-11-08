// SPDX-License-Identifier: GPL-3.0-or-later
// Persistent-reference lookup follows WGCNE's KeychainConfigurationStore. Ownership
// is checked on returned attributes, never by mixing item filters into that lookup.
import Foundation
import LocalAuthentication
import Security

struct ConfigurationIdentity: Equatable {
    static let service = "dev.idy.sing_box_flutter.configuration"
    static let maximumConfigurationBytes = 16 * 1024 * 1024
    let appGroup: String
    let providerBundleIdentifier: String
    let accessGroup: String

    // Separators cannot occur in either validated identifier. The extension derives
    // this account from its own bundle ID, rather than trusting an account in preferences.
    var account: String { appGroup + "/" + providerBundleIdentifier }

    init(appGroup: String, providerBundleIdentifier: String, accessGroup: String) throws {
        guard appGroup.hasPrefix("group."), Self.validIdentifier(appGroup),
              Self.validIdentifier(providerBundleIdentifier) else { throw TunnelFailure.invalidArguments }
        guard Self.validIdentifier(accessGroup) else { throw TunnelFailure.invalidKeychainGroup }
        self.appGroup = appGroup
        self.providerBundleIdentifier = providerBundleIdentifier
        self.accessGroup = accessGroup
    }

    static func validIdentifier(_ value: String) -> Bool {
        let allowed = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-")
        return !value.isEmpty && value.utf8.count <= 255 && value.contains(".")
            && value.unicodeScalars.allSatisfy { allowed.contains($0) }
    }
}

// Injection keeps Security failure/ownership tests independent of signing and never
// substitutes a mock Keychain in the app or Packet Tunnel builds.
protocol ConfigurationKeychainClient {
    func add(_ query: CFDictionary, result: inout CFTypeRef?) -> OSStatus
    func copyMatching(_ query: CFDictionary, result: inout CFTypeRef?) -> OSStatus
    func update(_ query: CFDictionary, attributes: CFDictionary) -> OSStatus
    func delete(_ query: CFDictionary) -> OSStatus
}

struct SystemConfigurationKeychainClient: ConfigurationKeychainClient {
    func add(_ query: CFDictionary, result: inout CFTypeRef?) -> OSStatus { SecItemAdd(query, &result) }
    func copyMatching(_ query: CFDictionary, result: inout CFTypeRef?) -> OSStatus { SecItemCopyMatching(query, &result) }
    func update(_ query: CFDictionary, attributes: CFDictionary) -> OSStatus { SecItemUpdate(query, attributes) }
    func delete(_ query: CFDictionary) -> OSStatus { SecItemDelete(query) }
}

/// The entire sing-box configuration, including WGC keys, is a non-synchronizable,
/// device-only item. VPN preferences contain only its opaque persistent reference.
final class KeychainConfigurationStore {
    let identity: ConfigurationIdentity
    private let client: ConfigurationKeychainClient

    init(identity: ConfigurationIdentity, client: ConfigurationKeychainClient = SystemConfigurationKeychainClient()) {
        self.identity = identity
        self.client = client
    }

    /// A missing initialize argument uses the app's current default Keychain group.
    /// This does not grant extension sharing: the host must explicitly entitle BOTH
    /// targets for that resolved group, or pass the same fully expanded shared
    /// `keychainAccessGroup` to initialize. There is no App Group/plaintext fallback.
    static func resolveAccessGroup(_ requested: String?, client: ConfigurationKeychainClient = SystemConfigurationKeychainClient()) throws -> String {
        if let requested, !ConfigurationIdentity.validIdentifier(requested) { throw TunnelFailure.invalidKeychainGroup }
        let account = "access-group-probe/" + UUID().uuidString
        let owner = baseQuery(account: account, accessGroup: requested)
        var query = owner
        query[kSecValueData as String] = Data([0])
        query[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        query[kSecReturnAttributes as String] = true
        var result: CFTypeRef?
        guard client.add(query as CFDictionary, result: &result) == errSecSuccess else { throw TunnelFailure.keychain }
        // This query names only our randomly generated probe; it never deletes a
        // foreign persistent reference, even if Security returns malformed attributes.
        let deletion = client.delete(owner as CFDictionary)
        guard deletion == errSecSuccess || deletion == errSecItemNotFound,
              let attributes = result as? [String: Any],
              let group = attributes[kSecAttrAccessGroup as String] as? String,
              ConfigurationIdentity.validIdentifier(group), requested == nil || group == requested,
              attributes[kSecAttrService as String] as? String == ConfigurationIdentity.service,
              attributes[kSecAttrAccount as String] as? String == account,
              attributes[kSecAttrAccessible as String] as? String == kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly as String,
              isNonSynchronizable(attributes) else { throw TunnelFailure.keychain }
        return group
    }

    func put(_ configuration: Data, sessionID: String? = nil) throws -> Data {
        guard !configuration.isEmpty, configuration.count <= ConfigurationIdentity.maximumConfigurationBytes else { throw TunnelFailure.storage }
        if let sessionID {
            guard UUID(uuidString: sessionID) != nil,
                  let payload = try? JSONSerialization.jsonObject(with: configuration) as? [String: Any],
                  payload["sessionID"] as? String == sessionID else { throw TunnelFailure.storage }
        }
        // Session-scoped items remain immutable while preferences still name the old reference.
        let account = sessionID.map { identity.account + "/" + $0 } ?? identity.account
        let owner = Self.baseQuery(account: account, accessGroup: identity.accessGroup)
        var query = owner
        query[kSecValueData as String] = configuration
        query[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        query[kSecReturnPersistentRef as String] = true
        var result: CFTypeRef?
        let status = client.add(query as CFDictionary, result: &result)
        let reference: Data
        switch status {
        case errSecSuccess:
            guard let value = result as? Data else { throw TunnelFailure.keychain }
            reference = value
        case errSecDuplicateItem:
            // Existing session items are immutable; legacy writes retain their old contract.
            guard sessionID == nil else { throw TunnelFailure.keychain }
            var lookup = owner
            lookup[kSecReturnAttributes as String] = true
            lookup[kSecReturnData as String] = true
            lookup[kSecReturnPersistentRef as String] = true
            lookup[kSecMatchLimit as String] = kSecMatchLimitOne
            guard client.copyMatching(lookup as CFDictionary, result: &result) == errSecSuccess,
                  let attributes = result as? [String: Any],
                  let value = attributes[kSecValuePersistentRef as String] as? Data else { throw TunnelFailure.keychain }
            _ = try Self.configurationData(from: result, identity: identity)
            reference = value
            guard client.update(owner as CFDictionary, attributes: [kSecValueData as String: configuration] as CFDictionary) == errSecSuccess else {
                throw TunnelFailure.keychain
            }
        default:
            throw TunnelFailure.keychain
        }
        // Check the exact item before handing its reference to NetworkExtension.
        guard try read(reference: reference) == configuration else { throw TunnelFailure.keychain }
        return reference
    }

    func read(reference: Data) throws -> Data {
        let query = try Self.readQuery(reference: reference)
        var result: CFTypeRef?
        guard client.copyMatching(query as CFDictionary, result: &result) == errSecSuccess else { throw TunnelFailure.keychain }
        return try Self.configurationData(from: result, identity: identity)
    }

    static func readQuery(reference: Data) throws -> [String: Any] {
        guard !reference.isEmpty, reference.count <= 4096 else { throw TunnelFailure.keychain }
        var query: [String: Any] = [
            kSecValuePersistentRef as String: reference,
            kSecReturnData as String: true,
            kSecReturnAttributes as String: true,
            kSecUseAuthenticationContext as String: noninteractiveContext(),
        ]
        #if os(macOS)
        query[kSecUseDataProtectionKeychain as String] = true
        #endif
        return query
    }

    static func configurationData(from result: CFTypeRef?, identity: ConfigurationIdentity) throws -> Data {
        guard let attributes = result as? [String: Any],
              attributes[kSecAttrService as String] as? String == ConfigurationIdentity.service,
              let account = attributes[kSecAttrAccount as String] as? String,
              attributes[kSecAttrAccessGroup as String] as? String == identity.accessGroup,
              attributes[kSecAttrAccessible as String] as? String == kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly as String,
              isNonSynchronizable(attributes),
              let data = attributes[kSecValueData as String] as? Data, !data.isEmpty,
              data.count <= ConfigurationIdentity.maximumConfigurationBytes else { throw TunnelFailure.keychain }
        if account != identity.account {
            guard let payload = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                  let sessionID = payload["sessionID"] as? String, UUID(uuidString: sessionID) != nil,
                  account == identity.account + "/" + sessionID else { throw TunnelFailure.keychain }
        }
        return data
    }

    private static func isNonSynchronizable(_ attributes: [String: Any]) -> Bool {
        let value = attributes[kSecAttrSynchronizable as String]
        return value == nil || value as? Bool == false
    }

    func remove(reference: Data) throws {
        var query = try Self.readQuery(reference: reference)
        var result: CFTypeRef?
        let status = client.copyMatching(query as CFDictionary, result: &result)
        if status == errSecItemNotFound { return }
        guard status == errSecSuccess else { throw TunnelFailure.keychain }
        _ = try Self.configurationData(from: result, identity: identity)
        query.removeValue(forKey: kSecReturnAttributes as String)
        query.removeValue(forKey: kSecReturnData as String)
        let deletion = client.delete(query as CFDictionary)
        guard deletion == errSecSuccess || deletion == errSecItemNotFound else { throw TunnelFailure.keychain }
    }

    private static func baseQuery(account: String, accessGroup: String?) -> [String: Any] {
        var query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: ConfigurationIdentity.service,
            kSecAttrAccount as String: account,
            kSecAttrSynchronizable as String: false,
            kSecUseDataProtectionKeychain as String: true,
            kSecUseAuthenticationContext as String: noninteractiveContext(),
        ]
        if let accessGroup { query[kSecAttrAccessGroup as String] = accessGroup }
        return query
    }

    private static func noninteractiveContext() -> LAContext {
        let context = LAContext()
        context.interactionNotAllowed = true
        return context
    }
}
