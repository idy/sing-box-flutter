// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import Libbox

enum NativeCore {
    static func verify(_ capabilities: CoreCapabilities) throws {
        guard LibboxVersion() == capabilities.version else { throw TunnelFailure.coreMismatch }
        #if SINGBOX_SELECTABLE_CORE
        guard capabilities.fingerprint != "legacy", LibboxCoreFingerprint() == capabilities.fingerprint else {
            throw TunnelFailure.coreMismatch
        }
        #else
        guard capabilities.fingerprint == "legacy" else { throw TunnelFailure.coreMismatch }
        #endif
    }

    static func verifySession(_ fingerprint: String?, capabilities: CoreCapabilities) throws {
        guard fingerprint == capabilities.fingerprint || (fingerprint == nil && capabilities.fingerprint == "legacy") else {
            throw TunnelFailure.coreMismatch
        }
    }

    static func setup(storage: TunnelStorage) throws {
        let directory = try storage.runtimeDirectory()
        let options = LibboxSetupOptions()
        options.basePath = directory.path
        options.workingPath = directory.appendingPathComponent("Working").path
        options.tempPath = directory.appendingPathComponent("Temp").path
        options.logMaxLines = 100
        options.debug = false
        options.crashReportSource = "PacketTunnel"
        options.commandServerSecret = UUID().uuidString
        options.oomKillerEnabled = true
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
