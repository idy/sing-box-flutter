// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation

/// Runs on macOS without Go, Flutter, VPN consent or an App Group entitlement.
@main
struct PreflightSmoke {
    static func main() throws {
        let manifest = try Data(contentsOf: URL(fileURLWithPath: CommandLine.arguments[1]))
        let full = try CoreCapabilities.decode(manifest)
        var minimalFeatures = Dictionary(uniqueKeysWithValues: CoreCapabilities.groups.map { ($0, [String]()) })
        minimalFeatures["inbounds"] = ["tun"]
        minimalFeatures["outbounds"] = ["direct"]
        minimalFeatures["endpoints"] = ["wgc"]
        let minimal = CoreCapabilities(schema: 1, version: "1.14.4", fingerprint: String(repeating: "a", count: 64), features: minimalFeatures)
        let valid = #"{"inbounds":[{"type":"tun","address":["172.19.0.1/30"],"auto_route":true}],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}"#
        try ConfigPreflight.check(valid, capabilities: minimal, requireTUN: true)
        try ConfigPreflight.check("{}", capabilities: full)
        try rejects("{}", capabilities: full, requireTUN: true)
        for invalid in ["[]", "null", "1", "{", #"{"inbounds":{}}"#, #"{"inbounds":[null]}"#, #"{"inbounds":[{"type":1}]}"#, #"{"dns":{"servers":{}}}"#, #"{"outbounds":[{"type":"direct","server_port":true}]}"#, #"{"inbounds":[{"type":"tun","auto_route":"yes"}]}"#, #"{"inbounds":[{"type":"tun","mtu":1.5}]}"#, #"{"outbounds":[{"type":"direct","password":{}}]}"#, #"{"route":{"rules":{}}}"#, #"{"inbounds":[{"type":"tun"},{"type":"tun"}]}"#, #"{"experimental":{"clash_api":true}}"#, #"{"outbounds":[{"type":"vmess","transport":{"type":"unknown"}}]}"#, #"{"unexpected":true}"#] {
            try rejects(invalid, capabilities: full)
        }
        for group in ["inbounds", "outbounds", "endpoints", "services", "certificate_providers"] {
            try rejects("{\"\(group)\":[{\"type\":\"unknown\"}]}", capabilities: full)
            if let type = full.features[group]?.first(where: { !minimal.supports(group, $0) }) {
                try rejects("{\"\(group)\":[{\"type\":\"\(type)\"}]}", capabilities: minimal)
            }
        }
        try rejects(#"{"dns":{"servers":[{"type":"udp"}]}}"#, capabilities: minimal)
        try rejects(#"{"outbounds":[{"type":"direct","transport":{"type":"ws"}}]}"#, capabilities: minimal)
        for feature in ["utls", "reality", "ech"] {
            try rejects("{\"outbounds\":[{\"type\":\"direct\",\"tls\":{\"\(feature)\":{\"enabled\":true}}}]}", capabilities: minimal)
        }
        for api in ["clash_api", "v2ray_api"] {
            try rejects("{\"experimental\":{\"\(api)\":{}}}", capabilities: minimal)
        }
        try rejects(#"{"outbounds":[{"type":"direct","tls":{"certificate_provider":{"type":"tailscale"}}}]}"#, capabilities: minimal)
        try ConfigPreflight.check(#"{"outbounds":[{"type":"socks","udp_over_tcp":true,"domain_resolver":"dns","tls":{"certificate_provider":"shared"}}]}"#, capabilities: full)
        try ConfigPreflight.check(#"{"route":{"rule_set":[{"type":"remote","tag":"cn"}],"rules":[{"rule_set":["cn"],"outbound":"direct"},{"rule_set":"cn","outbound":"direct"}]},"inbounds":[{"type":"tun","routing_mark":"0x1"}]}"#, capabilities: full)
        try rejects(#"{"route":{"rules":[{"rule_set":1}]}}"#, capabilities: full)
        try scopedPrimitiveChecks(full)
        try transportChecks(full)
        try optionalTLSChecks(full)
        try inlineACMEChecks(full)
        try implicitDirectChecks(full)
        try opaqueMapChecks(full)
        // Deliberately beyond preflight: references, key material and unknown
        // protocol fields must still be rejected by the extension's real core.
        try ConfigPreflight.check(#"{"inbounds":[{"type":"tun"}],"endpoints":[{"type":"wgc","private_key":"not-a-key"}],"route":{"final":"missing"}}"#, capabilities: minimal, requireTUN: true)
        do {
            _ = try CoreCapabilities.decode(Data("{}".utf8))
            fatalError("Malformed manifest accepted")
        } catch let failure as TunnelFailure { precondition(failure.code == "core_mismatch") }
        let oldSession = Data(#"{"version":1,"sessionID":"old","config":"{}","name":"old"}"#.utf8)
        let decodedSession = try JSONDecoder().decode(TunnelConfiguration.self, from: oldSession)
        precondition(decodedSession.coreFingerprint == nil)
        let event = TunnelStatus("error", message: String(repeating: "a", count: 1000)).event
        precondition(event["type"] as? String == "status" && (event["message"] as? String)?.count == 240)
        print("Foundation preflight: scoped shapes, transports, TLS, ACME, implicit direct, opaque maps and existing contract checks passed.")
    }

    private static func scopedPrimitiveChecks(_ full: CoreCapabilities) throws {
        for key in [#""private-key-secret-sentinel""#, #"["PEM header","PEM body","PEM footer"]"#] {
            try ConfigPreflight.check(#"{"inbounds":[{"type":"tun"}],"outbounds":[{"type":"ssh","private_key":\#(key)}]}"#, capabilities: full, requireTUN: true)
        }
        for key in ["true", "1", "null", "{}", "[1]", #"["PEM",false]"#] {
            try rejects(#"{"outbounds":[{"type":"ssh","private_key":\#(key)}]}"#, capabilities: full)
        }
        try rejects(#"{"endpoints":[{"type":"wgc","private_key":["PEM"]}]}"#, capabilities: full)
        try rejects(#"{"outbounds":[{"type":"ssh","tls":{"reality":{"enabled":false,"private_key":["PEM"]}}}]}"#, capabilities: full)
        for group in ["inbounds", "outbounds", "endpoints", "services", "certificate_providers"] {
            if let type = full.features[group]?.first {
                try rejects("{\"\(group)\":[{\"type\":\"\(type)\",\"tag\":[\"one\",\"two\"]}]}", capabilities: full)
            }
        }
        for definition in [
            #"{"type":"remote","tag":"cn","url":"https://example.com/cn.srs"}"#,
            #"{"type":"remote","tag":["cn"],"url":"https://example.com/cn.srs"}"#,
            #"{"type":"remote","tag":["cn","us"],"url":"https://example.com/{tag}.srs","initial_path":"rules/{tag}.srs"}"#,
            #"{"type":"local","tag":["cn","us"],"path":"rules/{tag}.srs"}"#,
            #"{"tag":["inline"],"rules":[]}"#
        ] {
            try ConfigPreflight.check(#"{"route":{"rule_set":[\#(definition)]}}"#, capabilities: full)
        }
        for definition in [
            #"{"type":"remote","tag":["cn","us"],"url":"https://example.com/rules.srs"}"#,
            #"{"type":"remote","tag":["cn","us"],"url":"https://example.com/{tag}.srs","initial_path":"rules.srs"}"#,
            #"{"type":"local","tag":["cn","us"],"path":"rules.srs"}"#,
            #"{"tag":["cn","us"],"rules":[]}"#,
            #"{"type":"remote","tag":[],"url":"https://example.com/{tag}.srs"}"#,
            #"{"type":"remote","tag":["cn",1],"url":"https://example.com/{tag}.srs"}"#,
            #"{"type":"remote","tag":true,"url":"https://example.com/{tag}.srs"}"#,
            #"{"type":"remote","tag":"","url":"https://example.com/{tag}.srs"}"#,
            #"{"type":"remote","url":"https://example.com/{tag}.srs"}"#
        ] {
            try rejects(#"{"route":{"rule_set":[\#(definition)]}}"#, capabilities: full)
        }
        for definitions in [#""cn""#, #"["cn"]"#, "{}", "null"] {
            try rejects(#"{"route":{"rule_set":\#(definitions)}}"#, capabilities: full)
        }
        let openVPN = withFeatures(full, ["endpoints": ["openvpn-client"]])
        for fragment in ["0", "1300", "4294967295"] {
            try ConfigPreflight.check(#"{"inbounds":[{"type":"tun"}],"endpoints":[{"type":"openvpn-client","fragment":\#(fragment)}]}"#, capabilities: openVPN, requireTUN: true)
        }
        for fragment in ["true", "false", "-1", "1.5", "4294967296", #""1300""#, "null", "{}", "[]"] {
            try rejects(#"{"endpoints":[{"type":"openvpn-client","fragment":\#(fragment)}]}"#, capabilities: openVPN)
        }
        for fragment in ["true", "false"] {
            try ConfigPreflight.check(#"{"outbounds":[{"type":"http","tls":{"fragment":\#(fragment)}}]}"#, capabilities: full)
        }
        try rejects(#"{"outbounds":[{"type":"http","tls":{"fragment":1300}}]}"#, capabilities: full)
        try rejects(#"{"endpoints":[{"type":"openvpn-client","tls":{"fragment":1300}}]}"#, capabilities: openVPN)
    }

    private static func transportChecks(_ full: CoreCapabilities) throws {
        let selected = withFeatures(full, ["endpoints": ["openvpn-server", "wgc"], "transports": []])
        for field in ["", #", "transport":"plain""#, #", "transport":"dot""#, #", "transport":"doh""#, #", "transport":"""#] {
            try ConfigPreflight.check(#"{"inbounds":[{"type":"tun"}],"endpoints":[{"type":"openvpn-server","push":{"dns_servers":[{"addresses":["1.1.1.1"]\#(field)}]}}]}"#, capabilities: selected, requireTUN: true)
        }
        for transport in ["true", "1", "null", "[]", #"{"type":"ws"}"#] {
            try rejects(#"{"endpoints":[{"type":"openvpn-server","push":{"dns_servers":[{"transport":\#(transport)}]}}]}"#, capabilities: withFeatures(full, ["endpoints": ["openvpn-server"]]))
        }
        for push in ["true", "null", "[]", #"{"dns_servers":{}}"#, #"{"dns_servers":[1]}"#] {
            try rejects(#"{"endpoints":[{"type":"openvpn-server","push":\#(push)}]}"#, capabilities: selected)
        }
        try ConfigPreflight.check(#"{"outbounds":[{"type":"vmess","transport":{"type":"ws"}}]}"#, capabilities: full)
        try rejects(#"{"outbounds":[{"type":"vmess","transport":{"type":"ws"}}]}"#, capabilities: selected)
        try rejects(#"{"outbounds":[{"type":"vmess","transport":"ws"}]}"#, capabilities: full)
        for transport in [#""udp""#, #""custom-app-transport""#, #""""#] {
            try ConfigPreflight.check(#"{"endpoints":[{"type":"wgc","transport":\#(transport)}]}"#, capabilities: selected)
        }
        try rejects(#"{"endpoints":[{"type":"wgc","transport":{"type":"ws"}}]}"#, capabilities: full)
        try rejects(#"{"endpoints":[{"type":"wgc","transport":true}]}"#, capabilities: selected)
        // A nested type field cannot grant an unrelated dictionary WGC semantics.
        try rejects(#"{"outbounds":[{"type":"vmess","multiplex":{"type":"wgc","transport":"udp"}}]}"#, capabilities: full)
    }

    private static func optionalTLSChecks(_ full: CoreCapabilities) throws {
        let withoutTLS = withFeatures(full, ["tls": []])
        for outer in ["", #""enabled":true,"#, #""enabled":false,"#] {
            for feature in ["utls", "reality", "ech"] {
                for settings in ["{}", #"{"enabled":false}"#] {
                    try ConfigPreflight.check("{\"outbounds\":[{\"type\":\"http\",\"tls\":{\(outer)\"\(feature)\":\(settings)}}]}", capabilities: withoutTLS)
                }
                let active = "{\"outbounds\":[{\"type\":\"http\",\"tls\":{\(outer)\"\(feature)\":{\"enabled\":true}}}]}"
                try rejects(active, capabilities: withoutTLS)
                try ConfigPreflight.check(active, capabilities: full)
                for settings in ["false", "null", "[]", #""settings""#, #"{"enabled":"false"}"#, #"{"enabled":0}"#, #"{"enabled":1}"#, #"{"enabled":null}"#] {
                    try rejects("{\"outbounds\":[{\"type\":\"http\",\"tls\":{\(outer)\"\(feature)\":\(settings)}}]}", capabilities: full)
                }
            }
        }
        try rejects(#"{"outbounds":[{"type":"http","tls":{"enabled":"false","utls":{}}}]}"#, capabilities: withoutTLS)
    }

    private static func inlineACMEChecks(_ full: CoreCapabilities) throws {
        let selected = withFeatures(full, ["certificate_providers": ["acme"], "tls": []])
        let unselected = withFeatures(full, ["certificate_providers": []])
        for domain in [#""example.com""#, #"["example.com"]"#] {
            let config = #"{"inbounds":[{"type":"tun"},{"type":"trojan","tls":{"enabled":true,"acme":{"domain":\#(domain),"email":"admin@example.com"}}}]}"#
            try ConfigPreflight.check(config, capabilities: selected, requireTUN: true)
            try rejects(config, capabilities: unselected, requireTUN: true)
        }
        for acme in ["true", "null", "[]", #""acme""#, #"{"domain":1}"#, #"{"domain":["example.com",false]}"#] {
            try rejects(#"{"inbounds":[{"type":"trojan","tls":{"acme":\#(acme)}}]}"#, capabilities: selected)
        }
        try rejects(#"{"inbounds":[{"type":"trojan","tls":{"acme":{},"utls":{"enabled":true}}}]}"#, capabilities: selected)
        try rejects(#"{"inbounds":[{"type":"trojan","tls":{"acme":{},"certificate_provider":{"type":"unknown"}}}]}"#, capabilities: selected)
        // Mutual exclusion and issuance details remain engine checks.
        try ConfigPreflight.check(#"{"inbounds":[{"type":"trojan","tls":{"acme":{},"certificate_provider":{"type":"acme"}}}]}"#, capabilities: selected)
    }

    private static func implicitDirectChecks(_ full: CoreCapabilities) throws {
        let selected = withFeatures(full, ["outbounds": ["http"], "endpoints": ["wgc"]])
        for config in ["{}", #"{"outbounds":[]}"#, #"{"route":{"final":""}}"#, #"{"endpoints":[{"type":"wgc","tag":"vpn"}]}"#, #"{"endpoints":[{"type":"wgc","tag":"vpn"}],"route":{"final":""}}"#] {
            try rejects(config, capabilities: selected)
            try ConfigPreflight.check(config, capabilities: full)
        }
        try rejects(#"{"inbounds":[{"type":"tun"}],"outbounds":[],"dns":{"servers":[{"type":"udp","server":"1.1.1.1"}]}}"#, capabilities: selected, requireTUN: true)
        try ConfigPreflight.check(#"{"outbounds":[{"type":"http","tag":"proxy"}]}"#, capabilities: selected)
        try ConfigPreflight.check(#"{"outbounds":[{"type":"http","tag":"proxy"}],"endpoints":[{"type":"wgc","tag":"vpn"}]}"#, capabilities: selected)
        try ConfigPreflight.check(#"{"inbounds":[{"type":"tun"}],"endpoints":[{"type":"wgc","tag":"vpn"}],"route":{"final":"vpn"}}"#, capabilities: selected, requireTUN: true)
        // Reference resolution still belongs to native validation.
        try ConfigPreflight.check(#"{"route":{"final":"missing"}}"#, capabilities: selected)
        try rejects(#"{"outbounds":[{"type":"direct"}]}"#, capabilities: selected)
        try rejects(#"{"outbounds":[{"type":"http"}],"route":{"final":1}}"#, capabilities: selected)
    }

    private static func opaqueMapChecks(_ full: CoreCapabilities) throws {
        let opaque = #"{"type":"unselected","enabled":"application-value","tls":true,"transport":"custom","private_key":[1],"tag":[false],"fragment":"value","acme":true}"#
        for key in ["headers", "environment", "env", "predefined"] {
            try ConfigPreflight.check("{\"outbounds\":[{\"type\":\"http\",\"\(key)\":\(opaque)}]}", capabilities: full)
        }
        try ConfigPreflight.check(#"{"endpoints":[{"type":"wgc","transport":"custom","transport_options":\#(opaque)}]}"#, capabilities: full)
    }

    private static func withFeatures(_ base: CoreCapabilities, _ overrides: [String: [String]]) -> CoreCapabilities {
        CoreCapabilities(schema: base.schema, version: base.version, fingerprint: base.fingerprint,
                         features: base.features.merging(overrides) { _, selected in selected })
    }

    private static func rejects(_ config: String, capabilities: CoreCapabilities, requireTUN: Bool = false) throws {
        let sentinel = "private-key-secret-sentinel"
        do {
            try ConfigPreflight.check(config, capabilities: capabilities, requireTUN: requireTUN)
            fatalError("Invalid or uncompiled configuration accepted")
        } catch let failure as TunnelFailure {
            precondition(["invalid_config", "missing_tun", "multiple_tun"].contains(failure.code))
            precondition(!failure.message.contains(sentinel) && !failure.message.contains(config))
        }
    }
}
