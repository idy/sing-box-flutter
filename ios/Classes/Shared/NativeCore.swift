// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import Libbox

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
