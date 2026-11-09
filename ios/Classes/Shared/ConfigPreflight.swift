// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import CoreFoundation

/// Foundation-only JSON structure and compiled-feature checks. This does not
/// resolve references, parse keys/certificates, or run Go option constructors.
/// NativeCore.check and service creation in PacketTunnel remain authoritative.
enum ConfigPreflight {
    static func check(_ config: String, capabilities: CoreCapabilities, requireTUN: Bool = false) throws {
        do {
            guard let data = config.data(using: .utf8),
                  let root = try JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw TunnelFailure.invalidConfig }
            let objects: Set<String> = ["log", "dns", "ntp", "certificate", "route", "experimental"]
            let arrays: Set<String> = ["inbounds", "outbounds", "endpoints", "services", "certificate_providers", "http_clients", "network_namespaces"]
            guard Set(root.keys).isSubset(of: objects.union(arrays).union(["$schema"])) else { throw TunnelFailure.invalidConfig }
            for key in objects where root[key] != nil { _ = try object(root[key]!) }
            for key in arrays where root[key] != nil { _ = try objectArray(root[key]!) }
            if let schema = root["$schema"], !(schema is String) { throw TunnelFailure.invalidConfig }
            for group in ["inbounds", "outbounds", "endpoints", "services", "certificate_providers"] {
                for component in try optionalArray(root[group]) { try typed(component, group: group, capabilities: capabilities) }
            }
            let inbounds = try optionalArray(root["inbounds"])
            let tunCount = inbounds.filter { $0["type"] as? String == "tun" }.count
            if tunCount > 1 { throw TunnelFailure(code: "multiple_tun", message: "Only one TUN inbound is supported on iOS.") }
            if requireTUN && tunCount != 1 { throw TunnelFailure(code: "missing_tun", message: "The configuration must start a TUN inbound on iOS.") }
            if let dns = root["dns"] as? [String: Any] {
                for server in try optionalArray(dns["servers"]) { try typed(server, group: "dns", capabilities: capabilities) }
                if dns["fakeip"] != nil { throw TunnelFailure.invalidConfig } // Removed legacy DNS syntax.
            }
            if let experimental = root["experimental"] as? [String: Any] {
                guard Set(experimental.keys).isSubset(of: ["cache_file", "clash_api", "v2ray_api", "debug"]) else { throw TunnelFailure.invalidConfig }
                for key in experimental.keys {
                    _ = try object(experimental[key]!)
                    if ["clash_api", "v2ray_api"].contains(key), !capabilities.supports("api", key) { throw TunnelFailure.invalidConfig }
                }
            }
            let outbounds = try optionalArray(root["outbounds"])
            let final = (root["route"] as? [String: Any])?["final"] as? String ?? ""
            // Endpoints become the default only through an explicit route.final.
            // Otherwise the engine creates direct when no outbound is configured.
            if !capabilities.supports("outbounds", "direct"), outbounds.isEmpty, final.isEmpty { throw TunnelFailure.invalidConfig }
            for (key, value) in root { try walk(value, capabilities: capabilities, path: [key]) }
        } catch let failure as TunnelFailure { throw failure }
        catch { throw TunnelFailure.invalidConfig } // Never include JSON parser diagnostics.
    }

    private static func typed(_ component: [String: Any], group: String, capabilities: CoreCapabilities) throws {
        guard let type = component["type"] as? String, !type.isEmpty, capabilities.supports(group, type) else { throw TunnelFailure.invalidConfig }
    }

    private static func walk(_ value: Any, capabilities: CoreCapabilities, path: [String] = [], endpointType: String? = nil) throws {
        if let array = value as? [Any] {
            for item in array { try walk(item, capabilities: capabilities, path: path + ["[]"], endpointType: endpointType) }
        } else if let dictionary = value as? [String: Any] {
            // Track the owning endpoint only at its schema boundary. Nested type
            // fields must not change how an unrelated option is interpreted.
            let componentType = dictionary["type"] as? String
            let endpointType = path == ["endpoints", "[]"] ? componentType : endpointType
            let ruleSetDefinition = path == ["route", "rule_set", "[]"]
            let openVPNPush = endpointType == "openvpn-server" && path == ["endpoints", "[]", "push"]
            let openVPNDNS = endpointType == "openvpn-server" && path == ["endpoints", "[]", "push", "dns_servers", "[]"]
            if ruleSetDefinition { try checkRuleSetTags(dictionary) }
            for (key, item) in dictionary {
                // Opaque application maps and WGC transport options have their own schemas.
                if ["transport_options", "headers", "predefined", "environment", "env"].contains(key) { continue }
                if key == "tls" {
                    let tls = try object(item)
                    for feature in ["utls", "reality", "ech"] {
                        if let settings = tls[feature] {
                            let settings = try object(settings)
                            if let enabled = settings["enabled"] {
                                guard isBool(enabled) else { throw TunnelFailure.invalidConfig }
                                if enabled as? Bool == true, !capabilities.supports("tls", feature) { throw TunnelFailure.invalidConfig }
                            }
                        }
                    }
                    // Inline ACME is deprecated, but remains available in this pin.
                    if let acme = tls["acme"] {
                        _ = try object(acme)
                        if !capabilities.supports("certificate_providers", "acme") { throw TunnelFailure.invalidConfig }
                    }
                } else if key == "transport" {
                    if openVPNDNS || (path == ["endpoints", "[]"] && endpointType == "wgc") {
                        guard item is String else { throw TunnelFailure.invalidConfig }
                    } else if let transport = item as? [String: Any] {
                        try typed(transport, group: "transports", capabilities: capabilities)
                    } else { throw TunnelFailure.invalidConfig }
                }
                // Validate common option envelopes and primitive shapes, without
                // pretending to duplicate the engine's per-protocol schema.
                if key == "certificate_provider" {
                    if let provider = item as? [String: Any] { try typed(provider, group: "certificate_providers", capabilities: capabilities) }
                    else if !(item is String) { throw TunnelFailure.invalidConfig }
                }
                if ["domain_resolver", "default_domain_resolver", "stun_domain_resolver"].contains(key), !(item is String) { _ = try object(item) }
                if key == "udp_over_tcp", !isBool(item) { _ = try object(item) }
                // Route/DNS rules reference rule sets by tag; route.rule_set defines objects.
                if key == "rule_set", path == ["route"] || (!(item is String) && !(item is [String])) { _ = try objectArray(item) }
                if key == "push", endpointType == "openvpn-server", path == ["endpoints", "[]"] { _ = try object(item) }
                if key == "dns_servers", openVPNPush { _ = try objectArray(item) }
                if objectKeys.contains(key) { _ = try object(item) }
                if arrayObjectKeys.contains(key) { _ = try objectArray(item) }
                if let shape = expectedPrimitiveShape(for: key, path: path, componentType: componentType) {
                    try validatePrimitive(item, as: shape)
                }
                try walk(item, capabilities: capabilities, path: path + [key], endpointType: endpointType)
            }
        }
    }

    private enum PrimitiveShape {
        case boolean, string, listableString
        case integer(maximum: Double?)
    }

    private static func expectedPrimitiveShape(for key: String, path: [String], componentType: String?) -> PrimitiveShape? {
        // Schema-specific overrides take precedence over the common defaults.
        if key == "fragment", path == ["endpoints", "[]"], componentType == "openvpn-client" {
            return .integer(maximum: Double(UInt32.max))
        }
        if (key == "tag" && path == ["route", "rule_set", "[]"]) ||
            (key == "private_key" && path == ["outbounds", "[]"] && componentType == "ssh") { return .listableString }
        if boolKeys.contains(key) { return .boolean }
        if stringKeys.contains(key) { return .string }
        if listableStringKeys.contains(key) { return .listableString }
        if integerKeys.contains(key) {
            return .integer(maximum: ["server_port", "listen_port", "peer_port"].contains(key) ? 65535 : nil)
        }
        return nil
    }

    private static func validatePrimitive(_ value: Any, as shape: PrimitiveShape) throws {
        switch shape {
        case .boolean:
            guard isBool(value) else { throw TunnelFailure.invalidConfig }
        case .string:
            guard value is String else { throw TunnelFailure.invalidConfig }
        case .listableString:
            guard value is String || value is [String] else { throw TunnelFailure.invalidConfig }
        case .integer(let maximum):
            guard let number = value as? NSNumber, !isBool(value), number.doubleValue >= 0,
                  number.doubleValue.rounded() == number.doubleValue else { throw TunnelFailure.invalidConfig }
            if let maximum, number.doubleValue > maximum { throw TunnelFailure.invalidConfig }
        }
    }

    private static func checkRuleSetTags(_ definition: [String: Any]) throws {
        let tags: [String]
        if let tag = definition["tag"] as? String { tags = [tag] }
        else if let list = definition["tag"] as? [String] { tags = list }
        else { throw TunnelFailure.invalidConfig }
        guard !tags.isEmpty, tags.allSatisfy({ !$0.isEmpty }) else { throw TunnelFailure.invalidConfig }
        if tags.count > 1 {
            switch definition["type"] as? String {
            case "remote":
                guard let url = definition["url"] as? String, url.contains("{tag}") else { throw TunnelFailure.invalidConfig }
                if let value = definition["initial_path"] {
                    guard let path = value as? String, path.isEmpty || path.contains("{tag}") else { throw TunnelFailure.invalidConfig }
                }
            case "local":
                guard let path = definition["path"] as? String, path.contains("{tag}") else { throw TunnelFailure.invalidConfig }
            default: throw TunnelFailure.invalidConfig // Inline rule sets support one tag.
            }
        }
    }

    private static func isBool(_ value: Any) -> Bool {
        guard let number = value as? NSNumber else { return false }
        return CFGetTypeID(number) == CFBooleanGetTypeID()
    }
    private static func object(_ value: Any) throws -> [String: Any] {
        guard let value = value as? [String: Any] else { throw TunnelFailure.invalidConfig }
        return value
    }
    private static func objectArray(_ value: Any) throws -> [[String: Any]] {
        guard let value = value as? [[String: Any]] else { throw TunnelFailure.invalidConfig }
        return value
    }
    private static func optionalArray(_ value: Any?) throws -> [[String: Any]] {
        guard let value else { return [] }
        return try objectArray(value)
    }
    private static let objectKeys: Set<String> = ["tls", "utls", "reality", "ech", "multiplex"]
    private static let arrayObjectKeys: Set<String> = ["rules", "peers"]
    private static let boolKeys: Set<String> = ["enabled", "disabled", "timestamp", "insecure", "disable_sni", "auto_route", "strict_route", "auto_redirect", "auto_detect_interface", "system_interface", "accept_routes", "accept_dns", "exit_node_allow_lan_access", "sniff", "sniff_override_destination", "domain_unmap", "udp_fragment", "udp_disable_domain_unmapping", "disable_cache", "disable_expire", "reverse_mapping", "store_fakeip", "store_dns", "store_rdrc", "independent_cache", "tcp_fast_open", "tcp_multi_path", "reuse_addr", "zero_rtt_handshake", "fragment", "record_fragment", "kernel_tx", "kernel_rx", "prefer_go"]
    private static let stringKeys: Set<String> = ["type", "tag", "server", "server_name", "server_key", "private_key", "public_key", "peer_public_key", "peer_endpoint", "password", "username", "uuid", "detour", "final", "default", "hostname", "state_directory", "stack", "dns_mode", "level", "output", "listen", "external_controller", "secret", "default_interface", "bind_interface", "interface_name", "network_namespace", "packet_encoding", "congestion_control", "flow", "fingerprint", "engine", "min_version", "max_version", "client_authentication"]
    private static let listableStringKeys: Set<String> = ["address", "alpn", "cipher_suites", "curve_preferences", "certificate", "certificate_path", "key", "key_path", "client_certificate", "client_certificate_path", "client_key", "client_key_path", "domain", "domain_suffix", "domain_keyword", "domain_regex", "ip_cidr", "source_ip_cidr", "route_address", "route_exclude_address", "route_address_set", "route_exclude_address_set", "dns_server", "outbounds"]
    private static let integerKeys: Set<String> = ["server_port", "listen_port", "peer_port", "mtu", "cache_capacity", "alter_id", "workers", "persistent_keepalive_interval"]
}
