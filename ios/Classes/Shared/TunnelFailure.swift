// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation

struct TunnelFailure: LocalizedError {
    let code: String
    let message: String
    var errorDescription: String? { message }

    static let notInitialized = TunnelFailure(code: "not_initialized", message: "Call initialize before using this method.")
    static let invalidArguments = TunnelFailure(code: "invalid_arguments", message: "Required arguments are missing or invalid.")
    static let invalidConfig = TunnelFailure(code: "invalid_config", message: "libbox rejected the configuration. Check it locally without sharing credentials.")
    static let busy = TunnelFailure(code: "busy", message: "Stop the current tunnel before starting or changing its configuration.")
    static let storage = TunnelFailure(code: "storage_error", message: "The shared configuration or App Group state could not be accessed.")
    static let keychain = TunnelFailure(code: "keychain_unavailable", message: "The device-only configuration Keychain item is unavailable. Check that the app and extension share the configured keychain-access-groups entitlement and unlock the device once after restart.")
    static let invalidKeychainGroup = TunnelFailure(code: "invalid_keychain_access_group", message: "keychainAccessGroup must be a fully expanded Keychain access group, including the signing prefix.")
    static let configurationRequired = TunnelFailure(code: "configuration_required", message: "Open the app and start the VPN once to save its configuration in the shared Keychain.")
    static let unsupported = TunnelFailure(code: "unsupported", message: "This feature is not supported by the Apple backend.")
}
