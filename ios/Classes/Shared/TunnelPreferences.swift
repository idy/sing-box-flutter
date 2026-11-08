// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import NetworkExtension

enum TunnelPreferences {
    static let schemaVersion = 2

    static func apply(to tunnel: NETunnelProviderProtocol, identity: ConfigurationIdentity, sessionID: String? = nil) {
        tunnel.providerBundleIdentifier = identity.providerBundleIdentifier
        var metadata: [String: Any] = [
            "appGroup": identity.appGroup,
            "keychainAccessGroup": identity.accessGroup,
            "schemaVersion": schemaVersion,
        ]
        if let sessionID { metadata["sessionID"] = sessionID }
        tunnel.providerConfiguration = metadata
        tunnel.serverAddress = "sing-box"
        tunnel.disconnectOnSleep = false
    }

    static func sessionID(in tunnel: NETunnelProviderProtocol) -> String? {
        guard let value = tunnel.providerConfiguration?["sessionID"] as? String,
              UUID(uuidString: value) != nil else { return nil }
        return value
    }

    static func matches(_ tunnel: NETunnelProviderProtocol, identity: ConfigurationIdentity) -> Bool {
        tunnel.providerBundleIdentifier == identity.providerBundleIdentifier
            && tunnel.providerConfiguration?["appGroup"] as? String == identity.appGroup
            && tunnel.providerConfiguration?["keychainAccessGroup"] as? String == identity.accessGroup
            && tunnel.providerConfiguration?["schemaVersion"] as? Int == schemaVersion
    }

    static func identity(from tunnel: NETunnelProviderProtocol, providerBundleIdentifier: String) throws -> ConfigurationIdentity {
        guard tunnel.providerBundleIdentifier == providerBundleIdentifier,
              tunnel.providerConfiguration?["schemaVersion"] as? Int == schemaVersion,
              let appGroup = tunnel.providerConfiguration?["appGroup"] as? String,
              let keychainAccessGroup = tunnel.providerConfiguration?["keychainAccessGroup"] as? String else {
            throw TunnelFailure.configurationRequired
        }
        return try ConfigurationIdentity(appGroup: appGroup, providerBundleIdentifier: providerBundleIdentifier, accessGroup: keychainAccessGroup)
    }
}
