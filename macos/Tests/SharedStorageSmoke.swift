// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation

/// Exercises macOS identifier acceptance without touching real App Group containers.
@main
struct SharedStorageSmoke {
    static func main() throws {
        let temporary = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: temporary) }
        for group in ["group.com.example.shared", "ABCDE12345.com.example.shared"] {
            let container = temporary.appendingPathComponent(UUID().uuidString, isDirectory: true)
            var requested: String?
            let storage = try TunnelStorage(appGroup: group, containerURL: { identifier in
                requested = identifier
                return container
            })
            precondition(requested == group && storage.appGroup == group)
            let saved = try storage.saveConfiguration("{}", name: "Storage smoke", coreFingerprint: String(repeating: "a", count: 64))
            let loaded = try storage.loadConfiguration()
            precondition(loaded.sessionID == saved.sessionID)
            let attributes = try FileManager.default.attributesOfItem(atPath: storage.root.path)
            precondition((attributes[.posixPermissions] as? NSNumber)?.intValue == 0o700)
        }
        var emptyLookupCalled = false
        do {
            _ = try TunnelStorage(appGroup: "", containerURL: { _ in
                emptyLookupCalled = true
                return temporary
            })
            fatalError("Empty App Group identifier accepted.")
        } catch let failure as TunnelFailure {
            precondition(failure.code == "app_group_unavailable" && !emptyLookupCalled)
        }
        do {
            _ = try TunnelStorage(appGroup: "ABCDE12345.com.example.shared", containerURL: { _ in nil })
            fatalError("Missing App Group container accepted.")
        } catch let failure as TunnelFailure {
            precondition(failure.code == "app_group_unavailable")
        }
        let inaccessible = temporary.appendingPathComponent("not-a-directory")
        try Data().write(to: inaccessible)
        do {
            _ = try TunnelStorage(appGroup: "ABCDE12345.com.example.shared", containerURL: { _ in inaccessible })
            fatalError("An inaccessible App Group container accepted.")
        } catch is TunnelFailure {
            fatalError("A valid Team-ID group was rejected before its container access was checked.")
        } catch {
            // A non-nil guessed macOS container URL does not prove usable access.
        }
        print("macOS shared storage: both App Group formats accepted; empty, missing and inaccessible containers rejected.")
    }
}
