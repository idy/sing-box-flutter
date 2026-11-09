// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import CoreFoundation

/// Foundation-only JSON structure and compiled-feature checks. This does not
/// resolve references, parse keys/certificates, or run Go option constructors.
/// NativeCore.check and service creation in PacketTunnel remain authoritative.
///
/// Like the native decoder, option field names match case-insensitively. Member
/// order is not available here, so when one field is spelled several ways, each
/// spelling is checked as if it were the one that takes effect.
enum ConfigPreflight {
    /// Object members with keys exactly as written. Reading them from NSDictionary
    /// keeps keys that Swift String equality would merge, such as "K" and U+212A.
    private typealias Members = [(key: String, value: Any)]

    static func check(_ config: String, capabilities: CoreCapabilities, requireTUN: Bool = false) throws {
        do {
            guard let data = config.data(using: .utf8) else { throw TunnelFailure.invalidConfig }
            let root = try members(JSONSerialization.jsonObject(with: data))
            let objects: Set<String> = ["log", "dns", "ntp", "certificate", "route", "experimental"]
            let arrays: Set<String> = ["inbounds", "outbounds", "endpoints", "services", "certificate_providers", "http_clients", "network_namespaces"]
            for (key, value) in root {
                let field = fieldName(key)
                if objects.contains(field) { _ = try members(value) }
                else if arrays.contains(field) { _ = try objectArray(value) }
                else if field != "$schema" || !(value is String) { throw TunnelFailure.invalidConfig }
            }
            for group in ["inbounds", "outbounds", "endpoints", "services", "certificate_providers"] {
                for list in values(root, group) {
                    for component in try objectArray(list) { try typed(component, group: group, capabilities: capabilities) }
                }
            }
            let inboundLists = try values(root, "inbounds").map(objectArray)
            for inbounds in inboundLists.isEmpty ? [[]] : inboundLists {
                let tunCount = inbounds.filter { discriminator($0) == "tun" }.count
                if tunCount > 1 { throw TunnelFailure(code: "multiple_tun", message: "Only one TUN inbound is supported on iOS.") }
                if requireTUN && tunCount != 1 { throw TunnelFailure(code: "missing_tun", message: "The configuration must start a TUN inbound on iOS.") }
            }
            for dns in values(root, "dns") {
                let dns = try members(dns)
                for list in values(dns, "servers") {
                    for server in try objectArray(list) { try typed(server, group: "dns", capabilities: capabilities) }
                }
                if !values(dns, "fakeip").isEmpty { throw TunnelFailure.invalidConfig } // Removed legacy DNS syntax.
            }
            for experimental in values(root, "experimental") {
                for (key, value) in try members(experimental) {
                    let field = fieldName(key)
                    guard ["cache_file", "clash_api", "v2ray_api", "debug"].contains(field) else { throw TunnelFailure.invalidConfig }
                    _ = try members(value)
                    if ["clash_api", "v2ray_api"].contains(field), !capabilities.supports("api", field) { throw TunnelFailure.invalidConfig }
                }
            }
            // Endpoints become the default only through an explicit route.final.
            // Otherwise the engine creates direct when no outbound is configured.
            let outboundLists = try values(root, "outbounds").map(objectArray)
            let finals = try values(root, "route").flatMap { values(try members($0), "final") }
            let mayLackOutbound = outboundLists.isEmpty || outboundLists.contains { $0.isEmpty }
            let mayLackFinal = finals.isEmpty || finals.contains { ($0 as? String ?? "").isEmpty }
            if !capabilities.supports("outbounds", "direct"), mayLackOutbound, mayLackFinal { throw TunnelFailure.invalidConfig }
            for (key, value) in root { try walk(value, capabilities: capabilities, path: [fieldName(key)]) }
        } catch let failure as TunnelFailure { throw failure }
        catch { throw TunnelFailure.invalidConfig } // Never include JSON parser diagnostics.
    }

    /// Option unions read their exact `type` discriminator; the native decoder
    /// rejects any other spelling as an unknown field of the selected options.
    private static func typed(_ component: Members, group: String, capabilities: CoreCapabilities) throws {
        guard let type = discriminator(component), !type.isEmpty, capabilities.supports(group, type),
              onlyExactSpelling(component, "type") else { throw TunnelFailure.invalidConfig }
    }

    private static func walk(_ value: Any, capabilities: CoreCapabilities, path: [String], endpointType: String? = nil) throws {
        if let array = value as? [Any] {
            for item in array { try walk(item, capabilities: capabilities, path: path + ["[]"], endpointType: endpointType) }
            return
        }
        guard value is NSDictionary else { return }
        let dictionary = try members(value)
        // Track the owning endpoint only at its schema boundary. Nested type
        // fields must not change how an unrelated option is interpreted.
        let componentType = discriminator(dictionary)
        let endpointType = path == ["endpoints", "[]"] ? componentType : endpointType
        let ruleSetDefinition = path == ["route", "rule_set", "[]"]
        let openVPNPush = endpointType == "openvpn-server" && path == ["endpoints", "[]", "push"]
        let openVPNDNS = endpointType == "openvpn-server" && path == ["endpoints", "[]", "push", "dns_servers", "[]"]
        if ruleSetDefinition { try checkRuleSetTags(dictionary) }
        for (key, item) in dictionary {
            let field = fieldName(key)
            // Opaque application maps and WGC transport options have their own schemas.
            if opaqueFields.contains(field) { continue }
            // Map keys are data, not field names: keep them exact and check only values.
            if mapFields.contains(field), item is NSDictionary {
                for (_, entry) in try members(item) {
                    try walk(entry, capabilities: capabilities, path: path + [field, "{}"], endpointType: endpointType)
                }
                continue
            }
            if field == "tls" {
                let tls = try members(item)
                for feature in ["utls", "reality", "ech"] {
                    for settings in values(tls, feature) {
                        for enabled in values(try members(settings), "enabled") {
                            guard isBool(enabled) else { throw TunnelFailure.invalidConfig }
                            if enabled as? Bool == true, !capabilities.supports("tls", feature) { throw TunnelFailure.invalidConfig }
                        }
                    }
                }
                // Inline ACME is deprecated, but remains available in this pin.
                for acme in values(tls, "acme") {
                    _ = try members(acme)
                    if !capabilities.supports("certificate_providers", "acme") { throw TunnelFailure.invalidConfig }
                }
            } else if field == "transport" {
                if openVPNDNS || (path == ["endpoints", "[]"] && endpointType == "wgc") {
                    guard item is String else { throw TunnelFailure.invalidConfig }
                } else {
                    try typed(members(item), group: "transports", capabilities: capabilities)
                }
            }
            // Validate common option envelopes and primitive shapes, without
            // pretending to duplicate the engine's per-protocol schema.
            if field == "certificate_provider" {
                if item is NSDictionary { try typed(members(item), group: "certificate_providers", capabilities: capabilities) }
                else if !(item is String) { throw TunnelFailure.invalidConfig }
            }
            if ["domain_resolver", "default_domain_resolver", "stun_domain_resolver"].contains(field), !(item is String) { _ = try members(item) }
            if field == "udp_over_tcp", !isBool(item) { _ = try members(item) }
            // Route/DNS rules reference rule sets by tag; route.rule_set defines objects.
            if field == "rule_set", path == ["route"] || (!(item is String) && !(item is [String])) { _ = try objectArray(item) }
            if field == "push", endpointType == "openvpn-server", path == ["endpoints", "[]"] { _ = try members(item) }
            if field == "dns_servers", openVPNPush { _ = try objectArray(item) }
            if objectKeys.contains(field) { _ = try members(item) }
            if arrayObjectKeys.contains(field) { _ = try objectArray(item) }
            if let shape = expectedPrimitiveShape(for: field, path: path, componentType: componentType) {
                try validatePrimitive(item, as: shape)
            }
            try walk(item, capabilities: capabilities, path: path + [field], endpointType: endpointType)
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

    private static func checkRuleSetTags(_ definition: Members) throws {
        // `type` and `tag` are exact union fields; the remaining fields fold case.
        guard onlyExactSpelling(definition, "type"), onlyExactSpelling(definition, "tag") else { throw TunnelFailure.invalidConfig }
        let value = exactValue(definition, "tag")
        let tags: [String]
        if let tag = value as? String { tags = [tag] }
        else if let list = value as? [String] { tags = list }
        else { throw TunnelFailure.invalidConfig }
        guard !tags.isEmpty, tags.allSatisfy({ !$0.isEmpty }) else { throw TunnelFailure.invalidConfig }
        if tags.count > 1 {
            func placeholders(_ field: String, allowEmpty: Bool) -> Bool {
                values(definition, field).allSatisfy { value in
                    guard let text = value as? String else { return false }
                    return (allowEmpty && text.isEmpty) || text.contains("{tag}")
                }
            }
            switch discriminator(definition) {
            case "remote":
                guard !values(definition, "url").isEmpty, placeholders("url", allowEmpty: false),
                      placeholders("initial_path", allowEmpty: true) else { throw TunnelFailure.invalidConfig }
            case "local":
                guard !values(definition, "path").isEmpty, placeholders("path", allowEmpty: false) else { throw TunnelFailure.invalidConfig }
            default: throw TunnelFailure.invalidConfig // Inline rule sets support one tag.
            }
        }
    }

    /// The native decoder's struct-field matching: ASCII letters fold case, and
    /// U+212A KELVIN SIGN and U+017F LATIN SMALL LETTER LONG S match k and s.
    private static func fieldName(_ key: String) -> String {
        var folded = String.UnicodeScalarView()
        for scalar in key.unicodeScalars {
            switch scalar.value {
            case 0x41...0x5A: folded.append(Unicode.Scalar(UInt8(scalar.value) + 0x20))
            case 0x212A: folded.append("k")
            case 0x17F: folded.append("s")
            default: folded.append(scalar)
            }
        }
        return String(folded)
    }

    /// Every member the native decoder assigns to `field`, in no particular order.
    private static func values(_ members: Members, _ field: String) -> [Any] {
        members.filter { fieldName($0.key) == field }.map { $0.value }
    }
    private static func isExact(_ key: String, _ field: String) -> Bool {
        key.unicodeScalars.elementsEqual(field.unicodeScalars)
    }
    private static func exactValue(_ members: Members, _ field: String) -> Any? {
        members.first { isExact($0.key, field) }?.value
    }
    private static func onlyExactSpelling(_ members: Members, _ field: String) -> Bool {
        members.allSatisfy { fieldName($0.key) != field || isExact($0.key, field) }
    }
    private static func discriminator(_ members: Members) -> String? { exactValue(members, "type") as? String }

    private static func isBool(_ value: Any) -> Bool {
        guard let number = value as? NSNumber else { return false }
        return CFGetTypeID(number) == CFBooleanGetTypeID()
    }
    private static func members(_ value: Any) throws -> Members {
        guard let dictionary = value as? NSDictionary else { throw TunnelFailure.invalidConfig }
        return try dictionary.map { member in
            guard let key = member.key as? String else { throw TunnelFailure.invalidConfig }
            return (key: key, value: member.value)
        }
    }
    private static func objectArray(_ value: Any) throws -> [Members] {
        guard let array = value as? [Any] else { throw TunnelFailure.invalidConfig }
        return try array.map(members)
    }
    private static let opaqueFields: Set<String> = ["transport_options", "headers", "predefined", "environment", "env"]
    /// Pinned option maps whose values are options (or strings) keyed by user data.
    private static let mapFields: Set<String> = ["servers", "torrc", "handshake_for_server_name", "fallback_for_alpn", "interface_address", "network_interface_address"]
    private static let objectKeys: Set<String> = ["tls", "utls", "reality", "ech", "multiplex"]
    private static let arrayObjectKeys: Set<String> = ["rules", "peers"]
    private static let boolKeys: Set<String> = ["enabled", "disabled", "timestamp", "insecure", "disable_sni", "auto_route", "strict_route", "auto_redirect", "auto_detect_interface", "system_interface", "accept_routes", "accept_dns", "exit_node_allow_lan_access", "sniff", "sniff_override_destination", "domain_unmap", "udp_fragment", "udp_disable_domain_unmapping", "disable_cache", "disable_expire", "reverse_mapping", "store_fakeip", "store_dns", "store_rdrc", "independent_cache", "tcp_fast_open", "tcp_multi_path", "reuse_addr", "zero_rtt_handshake", "fragment", "record_fragment", "kernel_tx", "kernel_rx", "prefer_go"]
    private static let stringKeys: Set<String> = ["type", "tag", "server", "server_name", "server_key", "private_key", "public_key", "peer_public_key", "peer_endpoint", "password", "username", "uuid", "detour", "final", "default", "hostname", "state_directory", "stack", "dns_mode", "level", "output", "listen", "external_controller", "secret", "default_interface", "bind_interface", "interface_name", "network_namespace", "packet_encoding", "congestion_control", "flow", "fingerprint", "engine", "min_version", "max_version", "client_authentication"]
    private static let listableStringKeys: Set<String> = ["address", "alpn", "cipher_suites", "curve_preferences", "certificate", "certificate_path", "key", "key_path", "client_certificate", "client_certificate_path", "client_key", "client_key_path", "domain", "domain_suffix", "domain_keyword", "domain_regex", "ip_cidr", "source_ip_cidr", "route_address", "route_exclude_address", "route_address_set", "route_exclude_address_set", "dns_server", "outbounds"]
    private static let integerKeys: Set<String> = ["server_port", "listen_port", "peer_port", "mtu", "cache_capacity", "alter_id", "workers", "persistent_keepalive_interval"]
}
