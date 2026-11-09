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
        try caseFoldingChecks(full, minimal)
        try precedenceChecks(full, minimal)
        try parserChecks(full, minimal)
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
        print("Foundation preflight: ordered parsing, member precedence, scoped shapes, field-name folding, transports, TLS, ACME, implicit direct, opaque maps and existing contract checks passed.")
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
                // Null resets the native pointer, like an omitted feature.
                for settings in ["{}", #"{"enabled":false}"#, "null"] {
                    try ConfigPreflight.check("{\"outbounds\":[{\"type\":\"http\",\"tls\":{\(outer)\"\(feature)\":\(settings)}}]}", capabilities: withoutTLS)
                }
                let active = "{\"outbounds\":[{\"type\":\"http\",\"tls\":{\(outer)\"\(feature)\":{\"enabled\":true}}}]}"
                try rejects(active, capabilities: withoutTLS)
                try ConfigPreflight.check(active, capabilities: full)
                for settings in ["false", "[]", #""settings""#, #"{"enabled":"false"}"#, #"{"enabled":0}"#, #"{"enabled":1}"#, #"{"enabled":null}"#] {
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
        for acme in ["true", "[]", #""acme""#, #"{"domain":1}"#, #"{"domain":["example.com",false]}"#] {
            try rejects(#"{"inbounds":[{"type":"trojan","tls":{"acme":\#(acme)}}]}"#, capabilities: selected)
        }
        try ConfigPreflight.check(#"{"inbounds":[{"type":"trojan","tls":{"acme":null}}]}"#, capabilities: unselected)
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

    /// Mirrors the pinned decoder: option fields fold ASCII case plus U+212A/U+017F,
    /// union discriminators and map keys stay exact, and the later spelling wins.
    private static func caseFoldingChecks(_ full: CoreCapabilities, _ minimal: CoreCapabilities) throws {
        let kelvin = "\u{212A}", longS = "\u{17F}"
        try ConfigPreflight.check(#"{"HTTP_Clients":[{"tag":"c","version":1,"TLS":{"ech":{"enabled":false}}}],"Route":{"Final":"fixture"},"$SCHEMA":"x"}"#, capabilities: minimal)
        try ConfigPreflight.check(#"{"INBOUNDS":[{"type":"tun"}],"Outbounds":[{"type":"direct"}]}"#, capabilities: minimal, requireTUN: true)
        try ConfigPreflight.check(#"{"\#(longS)ervices":[],"networ\#(kelvin)_namespaces":[]}"#, capabilities: full)
        for key in ["Unknown", "\u{130}nbounds", "\u{131}nbounds"] {
            try rejects(#"{"\#(key)":[]}"#, capabilities: full)
        }
        for config in [#"{"Inbounds":{}}"#, #"{"$Schema":1}"#, #"{"Inbounds":[{"type":"tun"},{"type":"tun"}]}"#, #"{"DNS":{"FakeIP":{}}}"#, #"{"Experimental":{"Unknown":{}}}"#] {
            try rejects(config, capabilities: full)
        }

        let limited = withFeatures(full, ["tls": [], "transports": ["grpc"], "dns": ["local"], "api": [], "certificate_providers": []])
        for config in [
            #"{"outbounds":[{"type":"vmess","Transport":{"type":"ws"}}]}"#,
            #"{"outbounds":[{"type":"vmess","transport":{"type":"grpc"},"TRANSPORT":{"type":"ws"}}]}"#,
            #"{"outbounds":[{"type":"http","TLS":{"UTLS":{"Enabled":true}}}]}"#,
            #"{"outbounds":[{"type":"http","tls":{"utl\#(longS)":{"enabled":true}}}]}"#,
            #"{"outbounds":[{"type":"http","tls":{"utls":{"enabled":false},"UTLS":{"enabled":true}}}]}"#,
            #"{"outbounds":[{"type":"http","tls":{"reality":{"enabled":false}},"TLS":{"reality":{"enabled":true}}}]}"#,
            #"{"outbounds":[{"type":"http","tls":{"ech":{"enabled":false,"ENABLED":true}}}]}"#,
            #"{"outbounds":[{"type":"http","tls":{"Certificate_Provider":{"type":"tailscale"}}}]}"#,
            #"{"DNS":{"Servers":[{"type":"udp"}]}}"#,
            #"{"Experimental":{"Clash_API":{}}}"#,
            #"{"inbounds":[{"type":"trojan","tls":{"Acme":{"domain":"example.com"}}}]}"#
        ] {
            try rejects(config, capabilities: limited)
        }
        for config in [
            #"{"outbounds":[{"type":"vmess","transport":{"type":"grpc"},"TRANSPORT":{"type":"ws"}}]}"#,
            #"{"outbounds":[{"type":"http","TLS":{"UTLS":{"Enabled":true},"Reality":{"Enabled":true},"ECH":{"Enabled":true}}}]}"#,
            #"{"DNS":{"Servers":[{"type":"udp"}]},"Experimental":{"Clash_API":{},"Cache_File":{}}}"#
        ] {
            try ConfigPreflight.check(config, capabilities: full)
        }
        try ConfigPreflight.check(#"{"outbounds":[{"type":"http","TLS":{"UTLS":{"Enabled":false}}}]}"#, capabilities: limited)
        try ConfigPreflight.check(#"{"inbounds":[{"type":"trojan","TLS":{"ACME":{"Domain":"example.com"}}}]}"#, capabilities: withFeatures(full, ["certificate_providers": ["acme"]]))
        try rejects(#"{"outbounds":[{"type":"direct"}],"Outbounds":[{"type":"vmess"}]}"#, capabilities: minimal)
        for config in [
            #"{"outbounds":[{"type":"http","TLS":{"UTLS":{"Enabled":"yes"}}}]}"#,
            #"{"outbounds":[{"type":"http","Server_Port":70000}]}"#,
            #"{"outbounds":[{"type":"ssh","private_\#(kelvin)ey":true,"private_Key":["a"]}]}"#,
            #"{"outbounds":[{"type":"ssh","private_\#(kelvin)ey":["a"],"private_Key":true}]}"#
        ] {
            try rejects(config, capabilities: full)
        }
        try ConfigPreflight.check(#"{"outbounds":[{"type":"ssh","Server_Port":22,"private_\#(kelvin)ey":["a"],"private_Key":"b"}]}"#, capabilities: full)

        let scoped = withFeatures(full, ["endpoints": ["wgc", "openvpn-client", "openvpn-server"], "transports": []])
        for config in [
            #"{"Endpoints":[{"type":"wgc","Transport":"udp"}]}"#,
            #"{"Endpoints":[{"type":"openvpn-server","Push":{"DNS_Servers":[{"Transport":"dot"}]}}]}"#,
            #"{"Endpoints":[{"type":"openvpn-client","Fragment":1300}]}"#,
            #"{"Route":{"Rule_Set":[{"type":"remote","tag":["a","b"],"URL":"https://example.com/{tag}.srs","Initial_Path":"r/{tag}.srs"}]}}"#
        ] {
            try ConfigPreflight.check(config, capabilities: scoped)
        }
        for config in [
            #"{"Endpoints":[{"type":"wgc","Transport":{"type":"ws"}}]}"#,
            #"{"Endpoints":[{"type":"openvpn-client","Fragment":true}]}"#,
            #"{"Route":{"Rule_Set":[{"type":"remote","tag":["a","b"],"URL":"https://example.com/a.srs"}]}}"#,
            #"{"route":{"rule_set":[{"type":"remote","tag":["a","b"],"url":"https://example.com/{tag}.srs","URL":"https://example.com/a.srs"}]}}"#
        ] {
            try rejects(config, capabilities: scoped)
        }
        // The native decoder rejects any other spelling of a union discriminator.
        for config in [
            #"{"outbounds":[{"Type":"direct"}]}"#,
            #"{"outbounds":[{"type":"direct","Type":"direct"}]}"#,
            #"{"outbounds":[{"type":"vmess","transport":{"type":"ws","Type":"ws"}}]}"#,
            #"{"dns":{"servers":[{"type":"udp","TYPE":"udp"}]}}"#,
            #"{"outbounds":[{"type":"http","tls":{"certificate_provider":{"type":"tailscale","Type":"tailscale"}}}]}"#,
            #"{"route":{"rule_set":[{"Type":"remote","tag":"a","url":"https://example.com/a.srs"}]}}"#,
            #"{"route":{"rule_set":[{"type":"remote","Tag":"a","url":"https://example.com/a.srs"}]}}"#
        ] {
            try rejects(config, capabilities: full)
        }
        let noDirect = withFeatures(full, ["outbounds": ["http"], "endpoints": ["wgc"]])
        try ConfigPreflight.check(#"{"Endpoints":[{"type":"wgc","tag":"vpn"}],"Route":{"Final":"vpn"}}"#, capabilities: noDirect)
        try rejects(#"{"Endpoints":[{"type":"wgc","tag":"vpn"}],"Route":{"FINAL":""}}"#, capabilities: noDirect)
        // The later Route spelling merges in and sets an empty final.
        try rejects(#"{"Endpoints":[{"type":"wgc","tag":"vpn"}],"route":{"final":"vpn"},"Route":{"Final":""}}"#, capabilities: noDirect)

        // Opaque and map-typed values keep exact keys; map values are still options.
        try ConfigPreflight.check(#"{"outbounds":[{"type":"http","Headers":{"TLS":true,"Transport":{"type":"unselected"},"type":"x","Host":"a","host":"b","\#(kelvin)ey":"c","Key":"d"}}]}"#, capabilities: full)
        try ConfigPreflight.check(#"{"endpoints":[{"type":"wgc","transport":"app","Transport_Options":{"tls":"opaque","Transport":{"type":"private-protocol"},"ech":{"enabled":true}}}]}"#, capabilities: minimal)
        try ConfigPreflight.check(#"{"inbounds":[{"type":"trojan","Fallback_For_ALPN":{"TLS":{"server":"192.0.2.1","Server_Port":443}}}],"services":[{"type":"ssm-api","Servers":{"Transport":"ss-in","TLS":"ss-in"}}],"outbounds":[{"type":"tor","Torrc":{"Enabled":"1"}}]}"#, capabilities: full)
        try rejects(#"{"inbounds":[{"type":"trojan","fallback_for_alpn":{"h2":{"Server_Port":70000}}}]}"#, capabilities: full)
    }

    /// Member order decides, as in the pinned decoder: the later member wins, null
    /// resets pointer and slice fields, and option unions first collapse a repeated
    /// exact key to its first position with its last value and drop empty values.
    private static func precedenceChecks(_ full: CoreCapabilities, _ minimal: CoreCapabilities) throws {
        let noTLS = withFeatures(full, ["tls": []])
        let http = #""type":"http","tag":"h","server":"192.0.2.1","server_port":8080"#
        let vmess = #""type":"vmess","tag":"v","server":"192.0.2.1","server_port":443,"uuid":"11111111-1111-4111-8111-111111111111""#
        let trojan = #""type":"trojan","tag":"t","listen_port":1"#
        func outbound(_ fields: String) -> String { "{\"outbounds\":[{\(fields)}]}" }
        func inbound(_ fields: String) -> String { "{\"inbounds\":[{\(fields)}]}" }

        // Later spellings win inside one object and across split objects; null resets.
        for tls in [
            #""enabled":true,"utls":{"enabled":true,"ENABLED":false}"#,
            #""enabled":true,"utls":{"enabled":true},"UTLS":{"enabled":false}"#,
            #""enabled":true,"utls":{"enabled":true},"UTLS":null"#
        ] {
            try ConfigPreflight.check(outbound(#"\#(http),"tls":{\#(tls)}"#), capabilities: noTLS)
        }
        for tls in [#""enabled":true,"utls":{"ENABLED":false,"enabled":true}"#, #""enabled":true,"UTLS":null,"utls":{"enabled":true}"#] {
            try rejects(outbound(#"\#(http),"tls":{\#(tls)}"#), capabilities: noTLS)
        }
        // A union collapses a repeated exact key, at any depth, to its first position
        // with its last value, then drops empty objects.
        for fields in [
            #"\#(http),"tls":{"utls":{"enabled":true}},"tls":{"enabled":true}"#,
            #"\#(http),"tls":{"enabled":true,"utls":{"enabled":true},"UTLS":{"enabled":false},"utls":{"enabled":true}}"#,
            #"\#(http),"tls":{"enabled":true,"utls":{"enabled":true},"utls":{}}"#,
            #"\#(http),"tls":{"enabled":true,"utls":{"enabled":true},"utls":null}"#
        ] {
            try ConfigPreflight.check(outbound(fields), capabilities: noTLS)
        }
        // HTTP clients decode TLS plainly, applying every member in order.
        for config in [
            #"{"http_clients":[{"tag":"c","version":1,"tls":{"enabled":true,"utls":{"enabled":true},"UTLS":{"enabled":false},"utls":{"enabled":true}}}]}"#,
            #"{"http_clients":[{"tag":"c","version":1,"tls":{"utls":{"enabled":true}},"tls":{"enabled":true}}]}"#
        ] {
            try rejects(config, capabilities: noTLS)
        }

        // Top-level options are plain too: repeated root keys apply in order.
        let noAPI = withFeatures(full, ["api": []])
        for config in [
            #"{"experimental":{"clash_api":{}},"experimental":null}"#,
            #"{"experimental":{"clash_api":{},"Clash_API":null}}"#,
            #"{"experimental":{"clash_api":{},"clash_api":null}}"#
        ] {
            try ConfigPreflight.check(config, capabilities: noAPI)
        }
        for config in [#"{"experimental":{"clash_api":{}},"experimental":{"cache_file":{}}}"#, #"{"experimental":{"v2ray_api":null,"V2Ray_API":{}}}"#] {
            try rejects(config, capabilities: noAPI)
        }
        for config in [#"{"inbounds":[],"Inbounds":[{"type":"tun"}]}"#, #"{"inbounds":[{"type":"tun"},{"type":"tun"}],"Inbounds":[{"type":"tun"}]}"#] {
            try ConfigPreflight.check(config, capabilities: minimal, requireTUN: true)
        }
        for config in [#"{"inbounds":[{"type":"tun"}],"inbounds":[]}"#, #"{"inbounds":[{"type":"tun"}],"Inbounds":null}"#] {
            try rejects(config, capabilities: minimal, requireTUN: true)
        }
        let noDirect = withFeatures(full, ["outbounds": ["http"], "endpoints": ["wgc"]])
        let endpoint = #""endpoints":[{"type":"wgc","tag":"vpn"}]"#
        try ConfigPreflight.check(#"{\#(endpoint),"route":{"final":""},"Route":{"final":"vpn"}}"#, capabilities: noDirect)
        try ConfigPreflight.check(#"{"outbounds":[],"Outbounds":[{\#(http)}]}"#, capabilities: noDirect)
        try rejects(#"{\#(endpoint),"route":{"final":"vpn"},"route":null}"#, capabilities: noDirect)
        try rejects(#"{"outbounds":[{\#(http)}],"outbounds":[]}"#, capabilities: noDirect)

        // Unions read the last non-null exact type, but every array entry still decodes.
        try ConfigPreflight.check(#"{"outbounds":[{"type":"unknown-x","type":"direct"}]}"#, capabilities: full)
        try ConfigPreflight.check(#"{"outbounds":[{"type":"direct","type":null}]}"#, capabilities: full)
        try rejects(#"{"outbounds":[{"type":1,"type":"direct"}]}"#, capabilities: full)
        try rejects(#"{"outbounds":[{"type":"unknown-x"}],"Outbounds":[]}"#, capabilities: full)
        // DNS options are a union whose nested servers arrive collapsed; any fakeip fails.
        try ConfigPreflight.check(#"{"dns":{"servers":[{"type":"unknown-x"}],"servers":[]}}"#, capabilities: full)
        for config in [
            #"{"dns":{"servers":[{"type":"unknown-x"}],"Servers":[]}}"#,
            #"{"dns":{"servers":[{"type":"udp","type":null,"server":"1.1.1.1"}]}}"#,
            #"{"dns":{"fakeip":null}}"#,
            #"{"dns":{"fakeip":{}},"dns":null}"#
        ] {
            try rejects(config, capabilities: full)
        }

        // Null resets pointer and slice fields without requiring any feature.
        let bare = withFeatures(full, ["tls": [], "transports": [], "api": [], "certificate_providers": []])
        for config in [
            #"{"log":null,"dns":null,"ntp":null,"certificate":null,"route":null,"experimental":null,"inbounds":null,"outbounds":null,"endpoints":null,"services":null,"certificate_providers":null,"http_clients":null,"network_namespaces":null,"$schema":null}"#,
            outbound(#"\#(vmess),"tls":null,"multiplex":null,"transport":null"#),
            outbound(#"\#(http),"tls":{"utls":null,"reality":null,"ech":null}"#),
            inbound(#"\#(trojan),"tls":{"acme":null,"certificate_provider":null,"ech":null,"reality":null}"#)
        ] {
            try ConfigPreflight.check(config, capabilities: bare)
        }
        // Every V2Ray transport type decodes; only the effective one is constructed.
        let grpcOnly = withFeatures(full, ["transports": ["grpc"]])
        for transport in [#""transport":{"type":"ws"},"Transport":{"type":"grpc"}"#, #""transport":{"type":"ws"},"Transport":null"#, #""transport":{}"#] {
            try ConfigPreflight.check(outbound(#"\#(vmess),\#(transport)"#), capabilities: grpcOnly)
        }
        try rejects(outbound(#"\#(vmess),"transport":{"type":"grpc"},"Transport":{"type":"ws"}"#), capabilities: grpcOnly)
        // Inline ACME needs the provider only when an effective, non-empty object remains.
        for tls in [#""enabled":true,"acme":{"domain":["example.com"]},"ACME":null"#, #""enabled":true,"acme":{}"#, #""enabled":true,"acme":{"domain":[]}"#] {
            try ConfigPreflight.check(inbound(#"\#(trojan),"tls":{\#(tls)}"#), capabilities: full)
        }
        try rejects(inbound(#"\#(trojan),"tls":{"enabled":true,"ACME":null,"acme":{"domain":"example.com"}}"#), capabilities: full)
        // A rule set's tag is its last exact member; its URL is the collapsed one.
        try ConfigPreflight.check(#"{"route":{"rule_set":[{"type":"remote","tag":"a","url":"https://e.example/a.srs","url":"https://e.example/{tag}.srs","tag":["a","b"]}]}}"#, capabilities: full)
    }

    /// The ordered parser accepts RFC 8259 only (the native decoder also accepts
    /// comments and trailing commas) and bounds nesting at 128 levels.
    private static func parserChecks(_ full: CoreCapabilities, _ minimal: CoreCapabilities) throws {
        // JSON escapes decode before field matching: t\u0075n is tun, \u212a folds to k.
        try ConfigPreflight.check(#"{"inbounds":[{"type":"t\u0075n"}],"networ\u212a_namespaces":[]}"#, capabilities: minimal, requireTUN: true)
        try ConfigPreflight.check(#"{"outbounds":[{"type":"direct","tag":"\ud83d\ude00 \ud800 \udc00x \ud800A \"\\\/\b\f\n\r\t"}]}"#, capabilities: minimal)
        // An unpaired surrogate becomes U+FFFD, as in Go, rather than disappearing.
        try rejects(#"{"inbounds":[{"type":"tun\ud800"}]}"#, capabilities: minimal, requireTUN: true)
        for invalid in ["", " ", "\n"] {
            do {
                try ConfigPreflight.check(invalid, capabilities: full)
                fatalError("Empty configuration accepted")
            } catch let failure as TunnelFailure { precondition(failure.code == "invalid_config") }
        }
        for invalid in [
            "{} {}", #"{"outbounds":[]}x"#, #"{"route"}"#, #"{'route':{}}"#, #"{"route":{"final":"a}}"#,
            #"{"inbounds":[{"type":"tun","mtu":01}]}"#, #"{"inbounds":[{"type":"tun","mtu":1.}]}"#,
            #"{"inbounds":[{"type":"tun","mtu":.5}]}"#, #"{"inbounds":[{"type":"tun","mtu":+1}]}"#,
            #"{"inbounds":[{"type":"tun","mtu":-}]}"#, #"{"inbounds":[{"type":"tun","mtu":1e}]}"#,
            #"{"inbounds":[{"type":"tun","mtu":NaN}]}"#, #"{"inbounds":[{"type":"tun","mtu":Infinity}]}"#,
            #"{"inbounds":[{"type":"tun","auto_route":True}]}"#, #"{"outbounds":[{"type":"direct","tag":"\x"}]}"#,
            #"{"outbounds":[{"type":"direct","tag":"\u12"}]}"#, "{\"outbounds\":[{\"type\":\"direct\",\"tag\":\"a\u{1}b\"}]}"
        ] {
            try rejects(invalid, capabilities: full)
        }
        try depthChecks(full)
    }

    /// The app and the extension run preflight on 512 KiB dispatch threads. The
    /// deepest accepted configuration must fit there in this unoptimized build;
    /// one level more is rejected (sing-box itself allows 10000).
    private static func depthChecks(_ full: CoreCapabilities) throws {
        func config(depth: Int) -> String {
            let nested = depth - 3 // Inside the root, the outbounds array and one outbound.
            return #"{"outbounds":[{"type":"http","x":"# + String(repeating: #"{"x":"#, count: nested - 1) + "{}" +
                String(repeating: "}", count: nested - 1) + "}]}"
        }
        var outcome: (deepest: Bool, deeper: Bool)?
        let done = DispatchSemaphore(value: 0)
        let thread = Thread {
            outcome = ((try? ConfigPreflight.check(config(depth: 128), capabilities: full)) != nil,
                       (try? ConfigPreflight.check(config(depth: 129), capabilities: full)) != nil)
            done.signal()
        }
        thread.stackSize = 512 * 1024
        thread.start()
        done.wait()
        precondition(outcome?.deepest == true && outcome?.deeper == false, "Nesting bound changed")
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
