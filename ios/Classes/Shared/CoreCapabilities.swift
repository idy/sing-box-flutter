// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation

/// Build metadata, never a runtime configuration or a source of credentials.
struct CoreCapabilities: Decodable {
    let schema: Int
    let version: String
    let fingerprint: String
    let features: [String: [String]]

    static let groups = Set(["inbounds", "outbounds", "endpoints", "dns", "transports", "tls", "api", "services", "certificate_providers"])

    static func decode(_ data: Data) throws -> CoreCapabilities {
        guard let value = try? JSONDecoder().decode(Self.self, from: data), value.schema == 1,
              !value.version.isEmpty, Set(value.features.keys) == groups,
              value.features.values.allSatisfy({ values in
                  values.allSatisfy { !$0.isEmpty } && Set(values).count == values.count
              }), value.fingerprint == "legacy" ||
                (value.fingerprint.count == 64 && value.fingerprint.allSatisfy { "0123456789abcdef".contains($0) }) else {
            throw TunnelFailure.coreMismatch
        }
        return value
    }

    static func load() throws -> CoreCapabilities {
        // CocoaPods copies the Shared resource bundle into each consuming app/extension.
        let owners = [Bundle(for: CapabilitiesBundleMarker.self), Bundle.main]
        for owner in owners {
            if let url = owner.url(forResource: "sing_box_flutter_capabilities", withExtension: "bundle"),
               let bundle = Bundle(url: url), let manifest = bundle.url(forResource: "core-capabilities", withExtension: "json") {
                do { return try decode(Data(contentsOf: manifest)) }
                catch { throw TunnelFailure.coreMismatch }
            }
        }
        throw TunnelFailure.coreMismatch
    }

    func supports(_ group: String, _ type: String) -> Bool { features[group]?.contains(type) == true }
}

private final class CapabilitiesBundleMarker: NSObject {}
