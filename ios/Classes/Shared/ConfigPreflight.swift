// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation
import CoreFoundation

/// Foundation-only JSON structure and compiled-feature checks. This does not
/// resolve references, parse keys/certificates, or run Go option constructors.
/// NativeCore.check and service creation in PacketTunnel remain authoritative.
///
/// The configuration is parsed like the pinned native decoder, comments and
/// trailing commas included, with every object member kept in source order.
/// Checks then follow its precedence: option field names match
/// case-insensitively, the later member of a field wins, and null resets pointer
/// and slice fields and leaves plain values unchanged. Option unions (protocol
/// entries, DNS, rules and rule sets) re-read their options through badjson,
/// which collapses a repeated exact key to its first position with its last
/// value and drops empty objects and arrays.
///
/// Two kinds of failure are checked. Decoding fails on any member the decoder
/// reads, even one a later member replaces: shape errors and types resolved
/// through compiled registries. Construction needs a compiled capability only
/// for options that take effect: TLS features, ACME, transports and APIs.
enum ConfigPreflight {
    /// Object members with keys exactly as written, in source order, repeats included.
    private typealias Members = [(key: String, value: Any)]
    private struct JSONObject { let members: Members }

    static func check(_ config: String, capabilities: CoreCapabilities, requireTUN: Bool = false) throws {
        do {
            var parser = Parser(config)
            guard let root = try parser.document() as? JSONObject else { throw TunnelFailure.invalidConfig }
            // Top-level options decode plainly: every member applies in source order.
            for (key, value) in root.members {
                let field = fieldName(key)
                guard rootObjects.contains(field) || rootArrays.contains(field) || field == "$schema" else { throw TunnelFailure.invalidConfig }
                if value is NSNull { continue } // Root fields are pointers, slices or a string.
                if rootObjects.contains(field) { _ = try object(value) }
                else if rootArrays.contains(field) { _ = try objectArray(value) }
                else if !(value is String) { throw TunnelFailure.invalidConfig }
            }
            try checkDecoded(root.members.map { Pending(value: $0.value, path: [fieldName($0.key)], union: false, endpointType: nil) }, capabilities)
            try checkConstructed(root, capabilities)
            let inbounds = effectiveArray(root.members, "inbounds")
            let tunCount = inbounds.filter { ($0 as? JSONObject).flatMap { discriminator($0.members) } == "tun" }.count
            if tunCount > 1 { throw TunnelFailure(code: "multiple_tun", message: "Only one TUN inbound is supported on iOS.") }
            if requireTUN && tunCount != 1 { throw TunnelFailure(code: "missing_tun", message: "The configuration must start a TUN inbound on iOS.") }
            // Endpoints become the default only through an explicit route.final.
            // Otherwise the engine creates direct when no outbound is configured.
            let final = merged(root.members, "route").flatMap { lastValue($0, "final") as? String } ?? ""
            if !capabilities.supports("outbounds", "direct"), effectiveArray(root.members, "outbounds").isEmpty, final.isEmpty {
                throw TunnelFailure.invalidConfig
            }
        } catch let failure as TunnelFailure { throw failure }
        catch { throw TunnelFailure.invalidConfig } // Never include parser diagnostics.
    }

    /// A value still to check. `union` is true when a badjson union has already
    /// re-encoded it; `endpointType` is the owning endpoint's type.
    private struct Pending {
        let value: Any
        let path: [String]
        let union: Bool
        let endpointType: String?
    }

    /// Decode-time checks on every value the decoder reads, visited with an
    /// explicit work list in source order so nesting never grows the stack
    /// (frames are large in unoptimized builds).
    private static func checkDecoded(_ start: [Pending], _ capabilities: CoreCapabilities) throws {
        var pending = Array(start.reversed())
        while let item = pending.popLast() {
            var children: [Pending] = []
            if let array = item.value as? [Any] {
                children = array.map { Pending(value: $0, path: item.path + ["[]"], union: item.union, endpointType: item.endpointType) }
            } else if let node = item.value as? JSONObject {
                children = try checkObject(node, capabilities, path: item.path, union: item.union, endpointType: item.endpointType)
            }
            pending.append(contentsOf: children.reversed())
        }
    }

    /// Checks one decoded object and returns the member values to visit next.
    private static func checkObject(_ node: JSONObject, _ capabilities: CoreCapabilities, path: [String], union: Bool, endpointType: String?) throws -> [Pending] {
        // What this object's own decoder reads, such as a union's `type`.
        let delivered = union ? unionMembers(node) : node.members
        // A union boundary re-reads its options, and everything inside, through badjson.
        let boundary = typedPaths[path] != nil || unionPaths.contains(path)
        let inUnion = union || boundary
        let members = inUnion ? unionMembers(node) : node.members
        let componentType = discriminator(delivered)
        // Track the owning endpoint only at its schema boundary. Nested type
        // fields must not change how an unrelated option is interpreted.
        let endpointType = path == ["endpoints", "[]"] ? componentType : endpointType
        let openVPNPush = endpointType == "openvpn-server" && path == ["endpoints", "[]", "push"]
        let openVPNDNS = endpointType == "openvpn-server" && path == ["endpoints", "[]", "push", "dns_servers", "[]"]
        let stringTransport = openVPNDNS || (path == ["endpoints", "[]"] && endpointType == "wgc")
        if let group = typedPaths[path] {
            try typed(delivered, group: group, capabilities: capabilities)
            // `tag` is decoded with `type` from every member the entry receives.
            for (key, value) in delivered where fieldName(key) == "tag" {
                guard isExact(key, "tag"), value is String || value is NSNull else { throw TunnelFailure.invalidConfig }
            }
        }
        if path == ["route", "rule_set", "[]"] { try checkRuleSet(delivered, members) }
        // Removed legacy DNS syntax: the decoder fails on any fakeip value, null included.
        if path == ["dns"], delivered.contains(where: { fieldName($0.key) == "fakeip" }) { throw TunnelFailure.invalidConfig }
        if path == ["experimental"] {
            for (key, item) in delivered {
                guard experimentalFields.contains(fieldName(key)), item is JSONObject || item is NSNull else { throw TunnelFailure.invalidConfig }
            }
        }
        var children: [Pending] = []
        for (key, item) in members {
            // The parent discriminator is checked above from `delivered`.
            if boundary, isExact(key, "type") { continue }
            let field = fieldName(key)
            // Opaque application maps and WGC transport options have their own schemas.
            if opaqueFields.contains(field) { continue }
            if let map = item as? JSONObject, mapFields.contains(field), !(field == "servers" && path == ["dns"]) {
                // Map keys are data, not field names: keep them exact and check only values.
                for (_, entry) in inUnion ? unionMembers(map) : map.members {
                    children.append(Pending(value: entry, path: path + [field, "{}"], union: inUnion, endpointType: endpointType))
                }
                continue
            }
            if item is NSNull {
                // Null resets pointers and slices and leaves plain values unchanged,
                // but this enum's decoder reads it as an unknown value.
                if field == "client_authentication" { throw TunnelFailure.invalidConfig }
            } else {
                try checkMember(field, item, inUnion: inUnion, path: path, componentType: componentType, endpointType: endpointType,
                                openVPNPush: openVPNPush, stringTransport: stringTransport, capabilities: capabilities)
            }
            children.append(Pending(value: item, path: path + [field], union: inUnion, endpointType: endpointType))
        }
        return children
    }

    /// Validate common option envelopes and primitive shapes, without
    /// pretending to duplicate the engine's per-protocol schema.
    private static func checkMember(_ field: String, _ item: Any, inUnion: Bool, path: [String], componentType: String?, endpointType: String?,
                                    openVPNPush: Bool, stringTransport: Bool, capabilities: CoreCapabilities) throws {
        if field == "servers", path == ["dns"] { _ = try objectArray(item) }
        if field == "transport" {
            if stringTransport {
                guard item is String else { throw TunnelFailure.invalidConfig }
            } else {
                // Each transport member decodes, so its type must be one the pin knows.
                let transport = try object(item)
                let delivered = inUnion ? unionMembers(transport) : transport.members
                try checkTypeSpelling(delivered)
                if let type = discriminator(delivered), !transportTypes.contains(type) { throw TunnelFailure.invalidConfig }
            }
        }
        if field == "certificate_provider" {
            // Each inline provider is decoded through the compiled registry.
            if let provider = item as? JSONObject {
                try typed(inUnion ? unionMembers(provider) : provider.members, group: "certificate_providers", capabilities: capabilities)
            } else if !(item is String) { throw TunnelFailure.invalidConfig }
        }
        if ["domain_resolver", "default_domain_resolver", "stun_domain_resolver"].contains(field), !(item is String) { _ = try object(item) }
        if field == "udp_over_tcp", !isBool(item) { _ = try object(item) }
        // Route/DNS rules reference rule sets by tag; route.rule_set defines objects.
        if field == "rule_set", path == ["route"] || (!(item is String) && !(item is [String])) { _ = try objectArray(item) }
        if field == "push", endpointType == "openvpn-server", path == ["endpoints", "[]"] { _ = try object(item) }
        if field == "dns_servers", openVPNPush { _ = try objectArray(item) }
        if objectKeys.contains(field) { _ = try object(item) }
        if arrayObjectKeys.contains(field) { _ = try objectArray(item) }
        if let shape = expectedPrimitiveShape(for: field, path: path, componentType: componentType) {
            try validatePrimitive(item, as: shape)
        }
    }

    /// An option the decoder keeps, with the objects merged into it in order.
    private struct Constructed {
        let objects: [JSONObject]
        let path: [String]
        let union: Bool
    }

    /// Construction-time capability checks, only on options that take effect.
    /// Objects after a field's last null merge; a slice keeps its last array,
    /// whose union entries build fresh options while plain entries merge into the
    /// same index of every array since the last null or empty array.
    private static func checkConstructed(_ root: JSONObject, _ capabilities: CoreCapabilities) throws {
        var pending = [Constructed(objects: [root], path: [], union: false)]
        while let item = pending.popLast() {
            let inUnion = item.union || typedPaths[item.path] != nil || unionPaths.contains(item.path)
            let members = item.objects.flatMap { inUnion ? unionMembers($0) : $0.members }
            try checkConstructedOptions(members, path: item.path, capabilities: capabilities)
            var fields: [String] = []
            for (key, _) in members where !fields.contains(fieldName(key)) { fields.append(fieldName(key)) }
            for field in fields where !opaqueFields.contains(field) {
                let values = members.filter { fieldName($0.key) == field }.map { $0.value }
                let path = item.path + [field]
                if values.last is JSONObject {
                    let start = values.lastIndex { $0 is NSNull }.map { $0 + 1 } ?? 0
                    let objects = values[start...].compactMap { $0 as? JSONObject }
                    if mapFields.contains(field) {
                        // A map entry replaces any earlier entry with the same exact key.
                        for (_, value) in collapsed(objects.flatMap { inUnion ? unionMembers($0) : $0.members }) {
                            if let object = value as? JSONObject { pending.append(Constructed(objects: [object], path: path + ["{}"], union: inUnion)) }
                        }
                    } else {
                        pending.append(Constructed(objects: objects, path: path, union: inUnion))
                    }
                } else if let last = values.last as? [Any], !last.isEmpty {
                    let start = values.lastIndex { $0 is NSNull || ($0 as? [Any])?.isEmpty == true }.map { $0 + 1 } ?? 0
                    let arrays = values[start...].compactMap { $0 as? [Any] }
                    let entryPath = path + ["[]"]
                    let fresh = typedPaths[entryPath] != nil || unionPaths.contains(entryPath)
                    for index in last.indices {
                        let objects = (fresh ? [last] : arrays).compactMap { $0.count > index ? $0[index] as? JSONObject : nil }
                        if !objects.isEmpty { pending.append(Constructed(objects: objects, path: entryPath, union: inUnion)) }
                    }
                }
            }
        }
    }

    private static func checkConstructedOptions(_ members: Members, path: [String], capabilities: CoreCapabilities) throws {
        guard let field = path.last else { return }
        let parent = path.dropLast().last
        if path.count == 2, parent == "experimental", ["clash_api", "v2ray_api"].contains(field), !capabilities.supports("api", field) {
            throw TunnelFailure.invalidConfig
        }
        if parent == "tls", ["utls", "reality", "ech"].contains(field), let enabled = lastValue(members, "enabled"),
           isBool(enabled), enabled as? Bool == true, !capabilities.supports("tls", field) {
            throw TunnelFailure.invalidConfig
        }
        // Inline ACME is deprecated, but remains available in this pin.
        if parent == "tls", field == "acme", !capabilities.supports("certificate_providers", "acme") { throw TunnelFailure.invalidConfig }
        // Every known V2Ray transport type decodes; only the effective one is constructed.
        if field == "transport" {
            guard let type = discriminator(members), !type.isEmpty, capabilities.supports("transports", type) else {
                throw TunnelFailure.invalidConfig
            }
        }
    }

    /// Option unions read `type` from every member they receive and keep the last
    /// non-null one. Another spelling stays in the options, where the decoder
    /// rejects it as an unknown field.
    private static func typed(_ delivered: Members, group: String, capabilities: CoreCapabilities) throws {
        try checkTypeSpelling(delivered)
        guard let type = discriminator(delivered), !type.isEmpty, capabilities.supports(group, type) else { throw TunnelFailure.invalidConfig }
    }

    private static func checkTypeSpelling(_ delivered: Members) throws {
        for (key, value) in delivered where fieldName(key) == "type" {
            guard isExact(key, "type"), value is String || value is NSNull else { throw TunnelFailure.invalidConfig }
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

    private static func checkRuleSet(_ delivered: Members, _ members: Members) throws {
        // `type` and `tag` are decoded plainly from every member and must be spelled
        // exactly; the remaining options come from the union's collapsed members.
        try checkTypeSpelling(delivered)
        var value: Any?
        for (key, item) in delivered where fieldName(key) == "tag" {
            guard isExact(key, "tag"), item is String || item is [String] || item is NSNull else { throw TunnelFailure.invalidConfig }
            if !(item is NSNull) { value = item }
        }
        let tags: [String]
        if let tag = value as? String { tags = [tag] }
        else if let list = value as? [String] { tags = list }
        else { throw TunnelFailure.invalidConfig }
        guard !tags.isEmpty, tags.allSatisfy({ !$0.isEmpty }) else { throw TunnelFailure.invalidConfig }
        if tags.count > 1 {
            func placeholder(_ field: String, required: Bool) -> Bool {
                guard let value = lastValue(members, field) else { return !required }
                guard let text = value as? String else { return false }
                return (!required && text.isEmpty) || text.contains("{tag}")
            }
            switch discriminator(delivered) {
            case "remote":
                guard placeholder("url", required: true), placeholder("initial_path", required: false) else { throw TunnelFailure.invalidConfig }
            case "local":
                guard placeholder("path", required: true) else { throw TunnelFailure.invalidConfig }
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

    /// Compares scalars: Swift String equality would match "K" with U+212A.
    private static func isExact(_ key: String, _ field: String) -> Bool {
        key.unicodeScalars.elementsEqual(field.unicodeScalars)
    }
    private static func discriminator(_ members: Members) -> String? {
        members.last { isExact($0.key, "type") && !($0.value is NSNull) }?.value as? String
    }
    /// A plain field keeps its last non-null value.
    private static func lastValue(_ members: Members, _ field: String) -> Any? {
        members.last { fieldName($0.key) == field && !($0.value is NSNull) }?.value
    }
    /// A slice of union entries keeps its last array; null resets it.
    private static func effectiveArray(_ members: Members, _ field: String) -> [Any] {
        members.last { fieldName($0.key) == field }?.value as? [Any] ?? []
    }
    /// The members a top-level pointer-to-struct field ends with: each object
    /// after the last null merges in source order. nil when absent or reset.
    private static func merged(_ members: Members, _ field: String) -> Members? {
        var result: Members?
        for (key, value) in members where fieldName(key) == field {
            if value is NSNull { result = nil }
            else if let object = value as? JSONObject { result = (result ?? []) + object.members }
        }
        return result
    }
    /// What a badjson union's options decoder receives for an object.
    private static func unionMembers(_ object: JSONObject) -> Members {
        withoutEmpty(collapsed(object.members))
    }
    /// badjson keeps a repeated exact key at its first position with its last value.
    private static func collapsed(_ members: Members) -> Members {
        var result: Members = []
        var positions: [[UInt8]: Int] = [:]
        for member in members {
            let key = Array(member.key.utf8)
            if let position = positions[key] {
                result[position].value = member.value
            } else {
                positions[key] = result.count
                result.append(member)
            }
        }
        return result
    }
    /// badjson drops members whose value is an empty object or array, recursively.
    private static func withoutEmpty(_ members: Members) -> Members {
        members.filter { !isEmptyUnionValue($0.value) }
    }
    private static func isEmptyUnionValue(_ value: Any) -> Bool {
        if let object = value as? JSONObject { return collapsed(object.members).allSatisfy { isEmptyUnionValue($0.value) } }
        if let array = value as? [Any] { return array.allSatisfy(isEmptyUnionValue) }
        return false
    }

    private static func isBool(_ value: Any) -> Bool {
        guard let number = value as? NSNumber else { return false }
        return CFGetTypeID(number) == CFBooleanGetTypeID()
    }
    private static func object(_ value: Any) throws -> JSONObject {
        guard let object = value as? JSONObject else { throw TunnelFailure.invalidConfig }
        return object
    }
    private static func objectArray(_ value: Any) throws -> [JSONObject] {
        guard let array = value as? [Any] else { throw TunnelFailure.invalidConfig }
        return try array.map(object)
    }

    /// Parses the pinned decoder's syntax, keeping every object member in order
    /// with keys as written. Like Go, it first blanks `#`, `//` and `/* */`
    /// comments outside strings, accepts trailing commas, and turns an unpaired
    /// UTF-16 surrogate escape into U+FFFD.
    private struct Parser {
        /// Far deeper than real configurations. Parsing recurses, and unoptimized
        /// builds use about 1 KiB of stack per nested object; 128 levels stay well
        /// inside the 512 KiB dispatch threads that run preflight.
        private static let maximumDepth = 128
        private var bytes: [UInt8]
        private var index = 0

        init(_ text: String) { bytes = Array(text.utf8) }

        mutating func document() throws -> Any {
            try blankComments()
            let value = try parseValue(depth: 0)
            skipWhitespace()
            // The scanner leaves a final lone slash as an unfinished comment, which it accepts.
            if index == bytes.count - 1, bytes[index] == UInt8(ascii: "/") { index += 1 }
            guard index == bytes.count else { throw TunnelFailure.invalidConfig }
            return value
        }

        /// The decoder's comment pass: strings are skipped by their quotes and
        /// escapes, and comment bytes become spaces except line breaks.
        private mutating func blankComments() throws {
            var position = 0
            func blank(_ range: Range<Int>) {
                for offset in range where bytes[offset] != 0x0A && bytes[offset] != 0x0D { bytes[offset] = 0x20 }
            }
            while position < bytes.count {
                let start = position
                switch bytes[position] {
                case UInt8(ascii: "\""):
                    position += 1
                    while position < bytes.count, bytes[position] != UInt8(ascii: "\"") {
                        position += bytes[position] == UInt8(ascii: "\\") ? 2 : 1
                    }
                    position += 1
                case UInt8(ascii: "#"):
                    while position < bytes.count, bytes[position] != 0x0A, bytes[position] != 0x0D { position += 1 }
                    blank(start..<position)
                case UInt8(ascii: "/") where position + 1 < bytes.count && bytes[position + 1] == UInt8(ascii: "/"):
                    while position < bytes.count, bytes[position] != 0x0A, bytes[position] != 0x0D { position += 1 }
                    blank(start..<position)
                case UInt8(ascii: "/") where position + 1 < bytes.count && bytes[position + 1] == UInt8(ascii: "*"):
                    position += 2
                    while position + 1 < bytes.count, bytes[position] != UInt8(ascii: "*") || bytes[position + 1] != UInt8(ascii: "/") {
                        position += 1
                    }
                    guard position + 1 < bytes.count else { throw TunnelFailure.invalidConfig }
                    position += 2
                    blank(start..<position)
                default:
                    position += 1
                }
            }
        }

        private mutating func parseValue(depth: Int) throws -> Any {
            skipWhitespace()
            guard index < bytes.count else { throw TunnelFailure.invalidConfig }
            switch bytes[index] {
            case UInt8(ascii: "{"): return try parseObject(depth: depth + 1)
            case UInt8(ascii: "["): return try parseArray(depth: depth + 1)
            case UInt8(ascii: "\""): return try parseString()
            case UInt8(ascii: "t"): try literal("true"); return NSNumber(value: true)
            case UInt8(ascii: "f"): try literal("false"); return NSNumber(value: false)
            case UInt8(ascii: "n"): try literal("null"); return NSNull()
            default: return try parseNumber()
            }
        }

        /// A trailing comma is accepted; a leading or repeated one is not.
        private mutating func parseObject(depth: Int) throws -> JSONObject {
            guard depth <= Self.maximumDepth else { throw TunnelFailure.invalidConfig }
            index += 1
            var members: Members = []
            while true {
                skipWhitespace()
                if consume(UInt8(ascii: "}")) { return JSONObject(members: members) }
                guard index < bytes.count, bytes[index] == UInt8(ascii: "\"") else { throw TunnelFailure.invalidConfig }
                let key = try parseString()
                skipWhitespace()
                guard consume(UInt8(ascii: ":")) else { throw TunnelFailure.invalidConfig }
                let value = try parseValue(depth: depth)
                members.append((key: key, value: value))
                skipWhitespace()
                if consume(UInt8(ascii: "}")) { return JSONObject(members: members) }
                guard consume(UInt8(ascii: ",")) else { throw TunnelFailure.invalidConfig }
            }
        }

        private mutating func parseArray(depth: Int) throws -> [Any] {
            guard depth <= Self.maximumDepth else { throw TunnelFailure.invalidConfig }
            index += 1
            var items: [Any] = []
            while true {
                skipWhitespace()
                if consume(UInt8(ascii: "]")) { return items }
                let item = try parseValue(depth: depth)
                items.append(item)
                skipWhitespace()
                if consume(UInt8(ascii: "]")) { return items }
                guard consume(UInt8(ascii: ",")) else { throw TunnelFailure.invalidConfig }
            }
        }

        private mutating func parseString() throws -> String {
            index += 1
            var output: [UInt8] = []
            while index < bytes.count {
                let byte = bytes[index]
                index += 1
                switch byte {
                case UInt8(ascii: "\""):
                    return String(decoding: output, as: UTF8.self)
                case UInt8(ascii: "\\"):
                    guard index < bytes.count else { throw TunnelFailure.invalidConfig }
                    let escape = bytes[index]
                    index += 1
                    switch escape {
                    case UInt8(ascii: "\""), UInt8(ascii: "\\"), UInt8(ascii: "/"): output.append(escape)
                    case UInt8(ascii: "b"): output.append(0x08)
                    case UInt8(ascii: "f"): output.append(0x0C)
                    case UInt8(ascii: "n"): output.append(0x0A)
                    case UInt8(ascii: "r"): output.append(0x0D)
                    case UInt8(ascii: "t"): output.append(0x09)
                    case UInt8(ascii: "u"):
                        guard var code = hex(at: index) else { throw TunnelFailure.invalidConfig }
                        index += 4
                        if (0xD800...0xDBFF).contains(code), let low = lowSurrogate() {
                            code = 0x10000 + ((code - 0xD800) << 10) + (low - 0xDC00)
                        } else if (0xD800...0xDFFF).contains(code) {
                            code = 0xFFFD
                        }
                        UTF8.encode(Unicode.Scalar(code) ?? "\u{FFFD}") { output.append($0) }
                    default: throw TunnelFailure.invalidConfig
                    }
                case 0..<0x20: throw TunnelFailure.invalidConfig
                default: output.append(byte)
                }
            }
            throw TunnelFailure.invalidConfig
        }

        /// Consumes a following `\uDC00`-`\uDFFF` escape; anything else stays unread.
        private mutating func lowSurrogate() -> UInt32? {
            guard index + 6 <= bytes.count, bytes[index] == UInt8(ascii: "\\"), bytes[index + 1] == UInt8(ascii: "u"),
                  let code = hex(at: index + 2), (0xDC00...0xDFFF).contains(code) else { return nil }
            index += 6
            return code
        }

        private func hex(at start: Int) -> UInt32? {
            guard start + 4 <= bytes.count else { return nil }
            var code: UInt32 = 0
            for byte in bytes[start..<start + 4] {
                let digit: UInt8
                switch byte {
                case UInt8(ascii: "0")...UInt8(ascii: "9"): digit = byte - UInt8(ascii: "0")
                case UInt8(ascii: "a")...UInt8(ascii: "f"): digit = byte - UInt8(ascii: "a") + 10
                case UInt8(ascii: "A")...UInt8(ascii: "F"): digit = byte - UInt8(ascii: "A") + 10
                default: return nil
                }
                code = code << 4 | UInt32(digit)
            }
            return code
        }

        private mutating func parseNumber() throws -> NSNumber {
            let start = index
            _ = consume(UInt8(ascii: "-"))
            if !consume(UInt8(ascii: "0")) {
                guard index < bytes.count, (UInt8(ascii: "1")...UInt8(ascii: "9")).contains(bytes[index]) else { throw TunnelFailure.invalidConfig }
                skipDigits()
            }
            var integral = true
            if consume(UInt8(ascii: ".")) {
                integral = false
                try requireDigits()
            }
            if consume(UInt8(ascii: "e")) || consume(UInt8(ascii: "E")) {
                integral = false
                if !consume(UInt8(ascii: "+")) { _ = consume(UInt8(ascii: "-")) }
                try requireDigits()
            }
            let text = String(decoding: bytes[start..<index], as: UTF8.self)
            if integral, let value = Int64(text) { return NSNumber(value: value) }
            if integral, let value = UInt64(text) { return NSNumber(value: value) }
            guard let value = Double(text) else { throw TunnelFailure.invalidConfig }
            return NSNumber(value: value)
        }

        private mutating func requireDigits() throws {
            let start = index
            skipDigits()
            guard index > start else { throw TunnelFailure.invalidConfig }
        }

        private mutating func skipDigits() {
            while index < bytes.count, (UInt8(ascii: "0")...UInt8(ascii: "9")).contains(bytes[index]) { index += 1 }
        }

        private mutating func literal(_ word: String) throws {
            let expected = Array(word.utf8)
            guard index + expected.count <= bytes.count, bytes[index..<index + expected.count].elementsEqual(expected) else {
                throw TunnelFailure.invalidConfig
            }
            index += expected.count
        }

        private mutating func consume(_ byte: UInt8) -> Bool {
            guard index < bytes.count, bytes[index] == byte else { return false }
            index += 1
            return true
        }

        private mutating func skipWhitespace() {
            while index < bytes.count {
                switch bytes[index] {
                case 0x20, 0x09, 0x0A, 0x0D: index += 1
                default: return
                }
            }
        }
    }

    private static let rootObjects: Set<String> = ["log", "dns", "ntp", "certificate", "route", "experimental"]
    private static let rootArrays: Set<String> = ["inbounds", "outbounds", "endpoints", "services", "certificate_providers", "http_clients", "network_namespaces"]
    private static let experimentalFields: Set<String> = ["cache_file", "clash_api", "v2ray_api", "debug"]
    /// The V2Ray transport types the pinned options decoder knows.
    private static let transportTypes: Set<String> = ["http", "ws", "quic", "grpc", "httpupgrade"]
    /// Option unions whose entries name a compiled component type.
    private static let typedPaths: [[String]: String] = [
        ["inbounds", "[]"]: "inbounds", ["outbounds", "[]"]: "outbounds", ["endpoints", "[]"]: "endpoints",
        ["services", "[]"]: "services", ["certificate_providers", "[]"]: "certificate_providers", ["dns", "servers", "[]"]: "dns",
    ]
    /// Other objects reachable from plain options whose decoders use badjson.
    /// HTTP clients decode their TLS and dial options plainly.
    private static let unionPaths: Set<[String]> = [["dns"], ["network_namespaces", "[]"], ["route", "rules", "[]"], ["route", "rule_set", "[]"]]
    private static let opaqueFields: Set<String> = ["transport_options", "headers", "predefined", "environment", "env"]
    /// Pinned option maps whose values are options (or strings) keyed by user data.
    private static let mapFields: Set<String> = ["servers", "torrc", "handshake_for_server_name", "fallback_for_alpn", "interface_address", "network_interface_address"]
    /// Pointer options; null resets them.
    private static let objectKeys: Set<String> = ["tls", "utls", "reality", "ech", "acme", "multiplex"]
    private static let arrayObjectKeys: Set<String> = ["rules", "peers"]
    private static let boolKeys: Set<String> = ["enabled", "disabled", "timestamp", "insecure", "disable_sni", "auto_route", "strict_route", "auto_redirect", "auto_detect_interface", "system_interface", "accept_routes", "accept_dns", "exit_node_allow_lan_access", "sniff", "sniff_override_destination", "domain_unmap", "udp_fragment", "udp_disable_domain_unmapping", "disable_cache", "disable_expire", "reverse_mapping", "store_fakeip", "store_dns", "store_rdrc", "independent_cache", "tcp_fast_open", "tcp_multi_path", "reuse_addr", "zero_rtt_handshake", "fragment", "record_fragment", "kernel_tx", "kernel_rx", "prefer_go"]
    private static let stringKeys: Set<String> = ["type", "tag", "server", "server_name", "server_key", "private_key", "public_key", "peer_public_key", "peer_endpoint", "password", "username", "uuid", "detour", "final", "default", "hostname", "state_directory", "stack", "dns_mode", "level", "output", "listen", "external_controller", "secret", "default_interface", "bind_interface", "interface_name", "network_namespace", "packet_encoding", "congestion_control", "flow", "fingerprint", "engine", "min_version", "max_version", "client_authentication"]
    private static let listableStringKeys: Set<String> = ["address", "alpn", "cipher_suites", "curve_preferences", "certificate", "certificate_path", "key", "key_path", "client_certificate", "client_certificate_path", "client_key", "client_key_path", "domain", "domain_suffix", "domain_keyword", "domain_regex", "ip_cidr", "source_ip_cidr", "route_address", "route_exclude_address", "route_address_set", "route_exclude_address_set", "dns_server", "outbounds"]
    private static let integerKeys: Set<String> = ["server_port", "listen_port", "peer_port", "mtu", "cache_capacity", "alter_id", "workers", "persistent_keepalive_interval"]
}
