// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import LocalAuthentication
import NetworkExtension
import Security

private struct TestFailure: Error { let message: String }

@main
struct KeychainStoreTests {
    private static var assertions = 0
    private static let identity = try! ConfigurationIdentity(appGroup: "group.dev.example.app", providerBundleIdentifier: "dev.example.app.PacketTunnel", accessGroup: "TEAM.dev.example.shared")
    private static let secret = Data("synthetic-private-configuration".utf8)

    static func main() throws {
        try testReferenceLookup()
        try testOwnershipAndProtection()
        try testWriteAndSecurityFailures()
        try testAccessGroupResolution()
        try testStorageAndMigration()
        try testPreferenceFailureKeepsPreviousConfiguration()
        try testPreferences()
        print("Apple security/storage contracts passed (\(assertions) assertions).")
    }

    private static func expect(_ value: @autoclosure () throws -> Bool, _ message: String) throws {
        assertions += 1
        guard try value() else { throw TestFailure(message: message) }
    }

    private static func expectFailure(_ code: String, _ operation: () throws -> Void) throws {
        do {
            try operation()
            throw TestFailure(message: "Expected credential-safe failure: " + code)
        } catch let failure as TunnelFailure {
            try expect(failure.code == code, "Unexpected platform error code")
            try expect(!failure.message.contains(String(decoding: secret, as: UTF8.self)), "Error exposed configuration bytes")
        }
    }

    private static var attributes: [String: Any] {
        [
            kSecAttrService as String: ConfigurationIdentity.service,
            kSecAttrAccount as String: identity.account,
            kSecAttrAccessGroup as String: identity.accessGroup,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
            kSecAttrSynchronizable as String: false,
            kSecValueData as String: secret,
        ]
    }

    private static func testReferenceLookup() throws {
        let reference = Data([1, 2, 3])
        let query = try KeychainConfigurationStore.readQuery(reference: reference)
        var keys: Set<String> = [kSecValuePersistentRef as String, kSecReturnData as String, kSecReturnAttributes as String, kSecUseAuthenticationContext as String]
        #if os(macOS)
        keys.insert(kSecUseDataProtectionKeychain as String)
        try expect(query[kSecUseDataProtectionKeychain as String] as? Bool == true, "macOS must use the data-protection Keychain")
        #endif
        try expect(Set(query.keys) == keys, "Persistent reference must not be combined with item-attribute filters")
        try expect(query[kSecValuePersistentRef as String] as? Data == reference, "Persistent reference changed")
        try expect((query[kSecUseAuthenticationContext as String] as? LAContext)?.interactionNotAllowed == true, "Keychain lookup may prompt")
        for invalid in [Data(), Data(repeating: 1, count: 4097)] {
            try expectFailure("keychain_unavailable") { _ = try KeychainConfigurationStore.readQuery(reference: invalid) }
        }
        try expect(try KeychainConfigurationStore.readQuery(reference: Data(repeating: 1, count: 4096)).count == keys.count, "Maximum reference rejected")
        // Exercise the real Security error boundary without requiring signed test entitlements.
        try expectFailure("keychain_unavailable") { _ = try KeychainConfigurationStore(identity: identity).read(reference: Data([0, 1, 2, 3])) }
    }

    private static func testOwnershipAndProtection() throws {
        try expect(try KeychainConfigurationStore.configurationData(from: attributes as CFDictionary, identity: identity) == secret, "Owned configuration not returned")
        for key in [kSecAttrService, kSecAttrAccount, kSecAttrAccessGroup, kSecAttrAccessible, kSecValueData] {
            var value = attributes
            value.removeValue(forKey: key as String)
            try expectFailure("keychain_unavailable") { _ = try KeychainConfigurationStore.configurationData(from: value as CFDictionary, identity: identity) }
        }
        let replacements: [(String, Any)] = [
            (kSecAttrService as String, "dev.idy.wgc_ne.configuration"),
            (kSecAttrAccount as String, identity.appGroup + "/dev.example.other.PacketTunnel"),
            (kSecAttrAccount as String, "group.dev.example.other/" + identity.providerBundleIdentifier),
            (kSecAttrAccessGroup as String, identity.accessGroup.lowercased()),
            (kSecAttrAccessGroup as String, identity.accessGroup + ".other"),
            (kSecAttrAccessible as String, kSecAttrAccessibleAfterFirstUnlock),
            (kSecAttrAccessible as String, kSecAttrAccessibleWhenUnlockedThisDeviceOnly),
            (kSecAttrSynchronizable as String, true),
            (kSecAttrSynchronizable as String, "false"),
            (kSecValueData as String, "not data"),
            (kSecValueData as String, Data()),
            (kSecValueData as String, Data(repeating: 1, count: ConfigurationIdentity.maximumConfigurationBytes + 1)),
        ]
        for (key, replacement) in replacements {
            var value = attributes
            value[key] = replacement
            try expectFailure("keychain_unavailable") { _ = try KeychainConfigurationStore.configurationData(from: value as CFDictionary, identity: identity) }
        }
        for result: CFTypeRef? in [nil, secret as CFData, [] as CFArray] {
            try expectFailure("keychain_unavailable") { _ = try KeychainConfigurationStore.configurationData(from: result, identity: identity) }
        }
        var absentSync = attributes
        absentSync.removeValue(forKey: kSecAttrSynchronizable as String)
        try expect(try KeychainConfigurationStore.configurationData(from: absentSync as CFDictionary, identity: identity) == secret, "Absent synchronizable attribute should mean false")
    }

    private static func testWriteAndSecurityFailures() throws {
        let client = MemoryKeychainClient()
        let store = KeychainConfigurationStore(identity: identity, client: client)
        let first = try store.put(secret)
        try expect(try store.read(reference: first) == secret, "Write/read round trip failed")
        try expect(client.lastAdd?[kSecAttrAccessible as String] as? String == kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly as String, "Configuration not device-only")
        try expect(client.lastAdd?[kSecAttrSynchronizable as String] as? Bool == false, "Configuration may synchronize")
        try expect(client.lastAdd?[kSecUseDataProtectionKeychain as String] as? Bool == true, "Legacy macOS Keychain used")
        let replacement = Data("replacement-configuration".utf8)
        try expect(try store.put(replacement) == first, "Stable owner item was replaced with a leaking obsolete item")
        try expect(try store.read(reference: first) == replacement, "Configuration was not updated")
        try expect(client.addedItems == 1 && client.updates == 1, "Old configurations accumulated")
        client.copyStatus = errSecInteractionNotAllowed
        try expectFailure("keychain_unavailable") { _ = try store.read(reference: first) }
        client.copyStatus = errSecSuccess
        client.updateStatus = errSecAuthFailed
        try expectFailure("keychain_unavailable") { _ = try store.put(secret) }
        client.updateStatus = errSecSuccess
        client.item?[kSecAttrAccount as String] = "foreign-owner"
        let updates = client.updates
        try expectFailure("keychain_unavailable") { _ = try store.put(secret) }
        try expect(client.updates == updates, "Foreign item was overwritten before ownership verification")
        let denied = MemoryKeychainClient()
        denied.addStatus = errSecMissingEntitlement
        let deniedStore = KeychainConfigurationStore(identity: identity, client: denied)
        try expectFailure("keychain_unavailable") { _ = try deniedStore.put(secret) }
        try expectFailure("storage_error") { _ = try deniedStore.put(Data()) }
        try expectFailure("storage_error") { _ = try deniedStore.put(Data(repeating: 0, count: ConfigurationIdentity.maximumConfigurationBytes + 1)) }
    }

    private static func testAccessGroupResolution() throws {
        for invalid in ["", "TEAM.invalid group", "TEAM.共享", "$(AppIdentifierPrefix)dev.example.shared", "TEAM.*", String(repeating: "a", count: 256)] {
            try expectFailure("invalid_keychain_access_group") { _ = try KeychainConfigurationStore.resolveAccessGroup(invalid, client: MemoryKeychainClient()) }
        }
        let client = MemoryKeychainClient()
        try expect(try KeychainConfigurationStore.resolveAccessGroup(nil, client: client) == client.defaultGroup, "Default group was guessed rather than resolved")
        try expect(client.item == nil, "Access-group probe was not deleted")
        try expect(try KeychainConfigurationStore.resolveAccessGroup(identity.accessGroup, client: client) == identity.accessGroup, "Explicit group not used")
        try expect(client.lastAdd?[kSecAttrAccessGroup as String] as? String == identity.accessGroup, "Explicit group dropped")
        client.addStatus = errSecMissingEntitlement
        try expectFailure("keychain_unavailable") { _ = try KeychainConfigurationStore.resolveAccessGroup(identity.accessGroup, client: client) }
        client.addStatus = errSecSuccess
        client.deleteStatus = errSecAuthFailed
        try expectFailure("keychain_unavailable") { _ = try KeychainConfigurationStore.resolveAccessGroup(nil, client: client) }
    }

    private static func testStorageAndMigration() throws {
        let container = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: container, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: container) }
        let client = TransactionalKeychainClient()
        let storage = try TunnelStorage(identity: identity, container: container, keychainClient: client)
        let legacyURL = storage.root.appendingPathComponent("configuration.json")
        let legacy = TunnelConfiguration(version: 1, sessionID: UUID().uuidString, config: String(decoding: secret, as: UTF8.self), name: "Legacy")
        try JSONEncoder().encode(legacy).write(to: legacyURL)
        try expect(try storage.legacyConfigurationForMigration()?.config == legacy.config, "Legacy host migration could not read profile")
        client.copyStatus = errSecItemNotFound
        try expectFailure("keychain_unavailable") { _ = try storage.loadConfiguration(reference: Data([1])) }
        client.copyStatus = errSecSuccess
        let saved = try storage.saveConfiguration(legacy.config, name: legacy.name)
        try expect(FileManager.default.fileExists(atPath: legacyURL.path), "Legacy file deleted before preferences can persist reference")
        try expect(try storage.loadConfiguration(reference: saved.reference, expectedSessionID: saved.configuration.sessionID).config == legacy.config, "Migrated configuration changed")
        try storage.removeLegacyConfiguration()
        try expect(!FileManager.default.fileExists(atPath: legacyURL.path), "Legacy plaintext not removed after migration")
        let updated = try storage.saveConfiguration("new-configuration", name: "New")
        try expect(!FileManager.default.fileExists(atPath: legacyURL.path), "New payload written to App Group")
        try expectFailure("stale_configuration") { _ = try storage.loadConfiguration(reference: updated.reference, expectedSessionID: saved.configuration.sessionID) }
        try storage.writeStatus(TunnelStatus("error", message: "Safe failure", sessionID: updated.configuration.sessionID))
        try expect(storage.readStatus()?.sessionID == updated.configuration.sessionID, "App Group status handoff lost")
        for validation in [true, false] {
            let runtime = try storage.runtimeDirectory(validation: validation)
            try expect(FileManager.default.fileExists(atPath: runtime.appendingPathComponent("Working").path), "Core working directory missing")
            try expect(FileManager.default.fileExists(atPath: runtime.appendingPathComponent("Temp").path), "Core temporary directory missing")
        }
        let statusData = try Data(contentsOf: storage.root.appendingPathComponent("status.json"))
        try expect(!String(decoding: statusData, as: UTF8.self).contains(String(decoding: secret, as: UTF8.self)), "Status file exposed payload")
    }

    private static func testPreferenceFailureKeepsPreviousConfiguration() throws {
        let container = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: container, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: container) }
        let client = TransactionalKeychainClient()
        let storage = try TunnelStorage(identity: identity, container: container, keychainClient: client)
        let previous = TunnelConfiguration(version: 2, sessionID: UUID().uuidString, config: "previous-configuration", name: "Previous")
        let previousReference = try KeychainConfigurationStore(identity: identity, client: client).put(JSONEncoder().encode(previous))
        let savedPreferences = NETunnelProviderProtocol()
        TunnelPreferences.apply(to: savedPreferences, identity: identity, sessionID: previous.sessionID)
        savedPreferences.passwordReference = previousReference
        let pending = try storage.saveConfiguration("pending-configuration", name: "Pending")
        try expect(pending.reference != previousReference, "Pending configuration overwrote the persisted Keychain item")
        let retained = try storage.loadConfiguration(reference: previousReference, expectedSessionID: previous.sessionID)
        try expect(retained.config == previous.config, "A preferences failure would invalidate the previous saved profile")
        try expect(savedPreferences.passwordReference == previousReference, "Pending write altered persisted preferences")
        try expect(client.items.count == 2, "Previous payload was removed before preferences committed")
        // Simulate saveToPreferences failing: only the pending item is discarded.
        try storage.removeConfiguration(reference: pending.reference)
        try expect(client.items.count == 1, "Failed pending payload was retained")
        try expect(try storage.loadConfiguration(reference: previousReference, expectedSessionID: previous.sessionID).config == previous.config, "Preferences rollback lost the previously working profile")
        // Once preferences commit and reload succeeds, the previous item can be retired.
        let committed = try storage.saveConfiguration("committed-configuration", name: "Committed")
        TunnelPreferences.apply(to: savedPreferences, identity: identity, sessionID: committed.configuration.sessionID)
        savedPreferences.passwordReference = committed.reference
        try storage.removeConfiguration(reference: previousReference)
        try storage.removeConfiguration(reference: previousReference)
        try expect(client.items.count == 1, "Committed profiles accumulated obsolete Keychain items")
        try expect(try storage.loadConfiguration(reference: committed.reference, expectedSessionID: committed.configuration.sessionID).config == "committed-configuration", "Committed configuration is unavailable")
        client.items[committed.reference]?[kSecAttrAccount as String] = identity.account + "/" + UUID().uuidString
        try expectFailure("keychain_unavailable") { try storage.removeConfiguration(reference: committed.reference) }
        try expect(client.items.count == 1, "Deletion removed an item whose ownership did not match its payload")
    }

    private static func testPreferences() throws {
        let tunnel = NETunnelProviderProtocol()
        let sessionID = UUID().uuidString
        TunnelPreferences.apply(to: tunnel, identity: identity, sessionID: sessionID)
        try expect(TunnelPreferences.matches(tunnel, identity: identity), "Preferences not recognized")
        try expect(try TunnelPreferences.identity(from: tunnel, providerBundleIdentifier: identity.providerBundleIdentifier) == identity, "Provider identity changed")
        try expect(TunnelPreferences.sessionID(in: tunnel) == sessionID, "Non-secret session metadata lost")
        try expect(Set(tunnel.providerConfiguration?.keys.map { $0 } ?? []) == ["appGroup", "keychainAccessGroup", "schemaVersion", "sessionID"], "Preferences contain unexpected payload metadata")
        try expect(tunnel.passwordReference == nil, "Consent-only profile should not contain a fabricated reference")
        try expectFailure("configuration_required") { _ = try TunnelPreferences.identity(from: tunnel, providerBundleIdentifier: "dev.foreign.PacketTunnel") }
        tunnel.providerConfiguration?["schemaVersion"] = 1
        try expectFailure("configuration_required") { _ = try TunnelPreferences.identity(from: tunnel, providerBundleIdentifier: identity.providerBundleIdentifier) }
        tunnel.providerConfiguration?["schemaVersion"] = 2
        tunnel.providerConfiguration?["keychainAccessGroup"] = "TEAM.invalid group"
        try expectFailure("invalid_keychain_access_group") { _ = try TunnelPreferences.identity(from: tunnel, providerBundleIdentifier: identity.providerBundleIdentifier) }
    }
}

private final class MemoryKeychainClient: ConfigurationKeychainClient {
    let defaultGroup = "TEAM.dev.example.default"
    let reference = Data([9, 8, 7])
    var item: [String: Any]?
    var lastAdd: [String: Any]?
    var addStatus = errSecSuccess
    var copyStatus = errSecSuccess
    var updateStatus = errSecSuccess
    var deleteStatus = errSecSuccess
    var addedItems = 0
    var updates = 0

    func add(_ query: CFDictionary, result: inout CFTypeRef?) -> OSStatus {
        let query = query as! [String: Any]
        lastAdd = query
        guard addStatus == errSecSuccess else { return addStatus }
        guard item == nil else { return errSecDuplicateItem }
        var attributes = query
        attributes[kSecAttrAccessGroup as String] = query[kSecAttrAccessGroup as String] ?? defaultGroup
        attributes[kSecValuePersistentRef as String] = reference
        item = attributes
        addedItems += 1
        if query[kSecReturnAttributes as String] as? Bool == true { result = attributes as CFDictionary }
        else { result = reference as CFData }
        return errSecSuccess
    }

    func copyMatching(_ query: CFDictionary, result: inout CFTypeRef?) -> OSStatus {
        guard copyStatus == errSecSuccess else { return copyStatus }
        guard let item else { return errSecItemNotFound }
        let query = query as! [String: Any]
        if let requested = query[kSecValuePersistentRef as String] as? Data, requested != reference { return errSecItemNotFound }
        result = item as CFDictionary
        return errSecSuccess
    }

    func update(_ query: CFDictionary, attributes: CFDictionary) -> OSStatus {
        guard updateStatus == errSecSuccess else { return updateStatus }
        guard item != nil else { return errSecItemNotFound }
        item?[kSecValueData as String] = (attributes as! [String: Any])[kSecValueData as String]
        updates += 1
        return errSecSuccess
    }

    func delete(_ query: CFDictionary) -> OSStatus {
        guard deleteStatus == errSecSuccess else { return deleteStatus }
        item = nil
        return errSecSuccess
    }
}

private final class TransactionalKeychainClient: ConfigurationKeychainClient {
    var items: [Data: [String: Any]] = [:]
    var copyStatus = errSecSuccess

    private func reference(for query: [String: Any]) -> Data? {
        if let reference = query[kSecValuePersistentRef as String] as? Data {
            return items[reference] == nil ? nil : reference
        }
        return items.first {
            $0.value[kSecAttrAccount as String] as? String == query[kSecAttrAccount as String] as? String
                && $0.value[kSecAttrService as String] as? String == query[kSecAttrService as String] as? String
                && $0.value[kSecAttrAccessGroup as String] as? String == query[kSecAttrAccessGroup as String] as? String
        }?.key
    }

    func add(_ query: CFDictionary, result: inout CFTypeRef?) -> OSStatus {
        let query = query as! [String: Any]
        guard reference(for: query) == nil else { return errSecDuplicateItem }
        let reference = Data(UUID().uuidString.utf8)
        var attributes = query
        attributes[kSecValuePersistentRef as String] = reference
        items[reference] = attributes
        result = reference as CFData
        return errSecSuccess
    }

    func copyMatching(_ query: CFDictionary, result: inout CFTypeRef?) -> OSStatus {
        guard copyStatus == errSecSuccess else { return copyStatus }
        guard let reference = reference(for: query as! [String: Any]), let item = items[reference] else { return errSecItemNotFound }
        result = item as CFDictionary
        return errSecSuccess
    }

    func update(_ query: CFDictionary, attributes: CFDictionary) -> OSStatus {
        guard let reference = reference(for: query as! [String: Any]) else { return errSecItemNotFound }
        items[reference]?[kSecValueData as String] = (attributes as! [String: Any])[kSecValueData as String]
        return errSecSuccess
    }

    func delete(_ query: CFDictionary) -> OSStatus {
        guard let reference = reference(for: query as! [String: Any]) else { return errSecItemNotFound }
        items.removeValue(forKey: reference)
        return errSecSuccess
    }
}
