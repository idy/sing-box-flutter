// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import SystemConfiguration

struct UserTunDNSPlan {
    let hasTun: Bool
    let servers: [String]
}

/// A store session owns temporary keys. Tests replace this boundary and never
/// invoke SystemConfiguration writes or change the host's resolver settings.
protocol UserTunDNSStore: AnyObject {
    func addTemporary(key: String, value: [String: Any]) throws
    func value(forKey key: String) -> [String: Any]?
    func remove(key: String) throws
    func close()
}

final class UserTunDNSController {
    typealias StoreFactory = (String, DispatchQueue, @escaping () -> Void) throws -> UserTunDNSStore
    private let queue = DispatchQueue(label: "dev.idy.sing_box_flutter.user-tun.dns")
    private let factory: StoreFactory
    private var store: UserTunDNSStore?
    private var key: String?
    private var expected: [String: Any]?
    private var failure: (() -> Void)?
    private var closed = false
    private var failed = false

    init(factory: @escaping StoreFactory = { key, queue, changed in
        try UserTunSystemDNSStore(key: key, queue: queue, changed: changed)
    }) { self.factory = factory }

    func install(servers: [String], onFailure: @escaping () -> Void) throws {
        try queue.sync {
            guard !closed, store == nil else { throw Self.configurationFailure }
            guard !servers.isEmpty else { return }
            let key = "State:/Network/Service/\(UUID().uuidString)/DNS"
            let value: [String: Any] = [
                kSCPropNetDNSServerAddresses as String: servers,
                kSCPropNetDNSSupplementalMatchDomains as String: [""],
                kSCPropNetDNSSearchOrder as String: NSNumber(value: 100000),
            ]
            let session = try factory(key, queue) { [weak self] in
                self?.queue.async { [weak self] in self?.configurationChanged() }
            }
            self.key = key
            expected = value
            failure = onFailure
            store = session
            do {
                try session.addTemporary(key: key, value: value)
            } catch {
                // Release the retained session even after a partially successful
                // write. Temporary ownership supplies crash/failure cleanup.
                session.close()
                store = nil
                expected = nil
                self.key = nil
                failure = nil
                throw Self.configurationFailure
            }
        }
    }

    private func configurationChanged() {
        guard !closed, !failed, let store, let key, let expected else { return }
        if let current = store.value(forKey: key) {
            guard NSDictionary(dictionary: current).isEqual(to: expected) else {
                fail()
                return
            }
            return
        }
        // An empty SystemConfiguration notification means configd restarted.
        // The client session reconnects, but temporary resolver state must be
        // registered again. Unexpected key removal uses the same recovery path.
        do { try store.addTemporary(key: key, value: expected) }
        catch { fail() }
    }

    private func fail() {
        failed = true
        failure?()
    }

    func close() throws {
        try queue.sync {
            guard !closed else { return }
            closed = true
            defer {
                store?.close()
                store = nil
                expected = nil
                key = nil
                failure = nil
            }
            if let store, let key, let expected, let current = store.value(forKey: key),
               NSDictionary(dictionary: current).isEqual(to: expected) {
                // Never overwrite or delete another writer's replacement value.
                try store.remove(key: key)
            }
        }
    }

    static let configurationFailure = TunnelFailure(
        code: "dns_configuration_failed",
        message: "The temporary macOS tunnel DNS configuration could not be maintained."
    )
}

private final class UserTunSystemDNSStore: UserTunDNSStore {
    private var store: SCDynamicStore?
    private let changed: () -> Void

    init(key: String, queue: DispatchQueue, changed: @escaping () -> Void) throws {
        self.changed = changed
        var context = SCDynamicStoreContext(version: 0, info: Unmanaged.passUnretained(self).toOpaque(), retain: nil, release: nil, copyDescription: nil)
        guard let session = SCDynamicStoreCreate(kCFAllocatorDefault, "sing_box_flutter user_tun DNS" as CFString, { _, _, info in
            guard let info else { return }
            Unmanaged<UserTunSystemDNSStore>.fromOpaque(info).takeUnretainedValue().changed()
        }, &context) else { throw UserTunDNSController.configurationFailure }
        store = session
        guard SCDynamicStoreSetNotificationKeys(session, [key] as CFArray, nil),
              SCDynamicStoreSetDispatchQueue(session, queue) else {
            close()
            throw UserTunDNSController.configurationFailure
        }
    }

    deinit { close() }

    func addTemporary(key: String, value: [String: Any]) throws {
        guard let store, SCDynamicStoreAddTemporaryValue(store, key as CFString, value as CFDictionary) else {
            throw UserTunDNSController.configurationFailure
        }
    }

    func value(forKey key: String) -> [String: Any]? {
        guard let store else { return nil }
        return SCDynamicStoreCopyValue(store, key as CFString) as? [String: Any]
    }

    func remove(key: String) throws {
        guard let store else { return }
        if !SCDynamicStoreRemoveValue(store, key as CFString), SCError() != kSCStatusNoKey {
            throw UserTunDNSController.configurationFailure
        }
    }

    func close() {
        guard let old = store else { return }
        SCDynamicStoreSetDispatchQueue(old, nil)
        store = nil // CFRelease closes the session and removes its temporary keys.
    }
}
