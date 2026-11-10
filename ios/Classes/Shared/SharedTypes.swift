// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation

struct TunnelFailure: LocalizedError {
    let code: String
    let message: String
    var errorDescription: String? { message }

    static let notInitialized = TunnelFailure(code: "not_initialized", message: "Call initialize before using this method.")
    static let invalidArguments = TunnelFailure(code: "invalid_arguments", message: "Required arguments are missing or invalid.")
    static let invalidConfig = TunnelFailure(code: "invalid_config", message: "The configuration is invalid or uses features unavailable in this core. Check it locally without sharing credentials.")
    static let busy = TunnelFailure(code: "busy", message: "Stop the current tunnel before starting or changing its configuration.")
    static let storage = TunnelFailure(code: "storage_error", message: "The App Group configuration or state could not be accessed.")
    static let coreMismatch = TunnelFailure(code: "core_mismatch", message: "The bundled core metadata does not match the installed core. Rebuild the app and extension with the same native artifact.")
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
    let coreFingerprint: String?
}
