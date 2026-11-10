// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestCapabilityGatesSharedImplementations(t *testing.T) {
	selected := map[string][]string{"outbounds": {"vmess", "direct"}, "inbounds": {"tun"}, "dns": {"udp"}, "endpoints": {"wgc"}, "services": {}, "certificate_providers": {}, "api": {}, "tls": {}, "transports": {}}
	for _, config := range []string{
		`{"outbounds":[{"type":"vmess","transport":{"type":"ws"}}]}`,
		`{"outbounds":[{"type":"vmess","tls":{"enabled":true,"utls":{"enabled":true}}}]}`,
		`{"outbounds":[{"type":"vmess","tls":{"reality":{"enabled":true}}}]}`,
		`{"outbounds":[{"type":"vmess","tls":{"ech":{"enabled":true}}}]}`,
		`{"experimental":{"clash_api":{}}}`,
		`{"experimental":{"v2ray_api":{}}}`,
		`{"services":[{"type":"api"}]}`,
		`{"certificate_providers":[{"type":"acme"}]}`,
		`{"outbounds":[{"type":"vmess","tls":{"certificate_provider":{"type":"acme"}}}]}`,
	} {
		if err := checkCapabilities(config, selected); err == nil {
			t.Fatalf("unselected feature accepted: %s", config)
		}
	}
	selected["transports"] = []string{"ws"}
	selected["tls"] = []string{"utls", "reality", "ech"}
	if err := checkCapabilities(`{"outbounds":[{"type":"vmess","transport":{"type":"ws"},"tls":{"enabled":true,"utls":{"enabled":true},"reality":{"enabled":true},"ech":{"enabled":true}}}]}`, selected); err != nil {
		t.Fatal(err)
	}
	if err := checkCapabilities(`{"endpoints":[{"type":"wgc","transport":"app","transport_options":{"tls":"opaque"}}]}`, selected); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityGateReadsNativeCommentSyntax(t *testing.T) {
	selected := map[string][]string{"outbounds": {"direct"}, "tls": {}}
	accepted := "{\"outbounds\":[{\"type\":\"direct\", /* c */ \"tag\":\"d\"},], // c\n\"route\":{}}"
	if err := checkCapabilities(accepted, selected); err != nil {
		t.Fatalf("native comment syntax rejected: %v", err)
	}
	for _, config := range []string{
		`{"outbounds":[{"type":"vmess" /* c */}],}`,
		"{\"outbounds\":[{\"type\":\"direct\",\"tls\":{\"utls\":{\"enabled\":true}}, // c\n}]}",
	} {
		if err := checkCapabilities(config, selected); err == nil {
			t.Fatalf("comments hid an unselected feature: %s", config)
		}
	}
}

func TestCapabilityGateRequiresSelectedImplicitDirect(t *testing.T) {
	for _, direct := range []bool{false, true} {
		selected := map[string][]string{"outbounds": {"vmess"}, "endpoints": {"wgc"}, "dns": {"udp"}, "inbounds": {"tun"}}
		if direct {
			selected["outbounds"] = append(selected["outbounds"], "direct")
		}
		for _, tc := range []struct {
			name, content string
			implicit      bool
		}{
			{"empty", `{}`, true},
			{"empty-outbounds", `{"outbounds":[]}`, true},
			{"tun-dns-only", `{"inbounds":[{"type":"tun"}],"dns":{"servers":[{"type":"udp"}]}}`, true},
			{"endpoint-without-final", `{"endpoints":[{"type":"wgc","tag":"vpn"}]}`, true},
			{"endpoint-empty-final", `{"endpoints":[{"type":"wgc","tag":"vpn"}],"route":{"final":""}}`, true},
			{"endpoint-final", `{"outbounds":[],"endpoints":[{"type":"wgc","tag":"vpn"}],"route":{"final":"vpn"}}`, false},
			{"first-outbound", `{"outbounds":[{"type":"vmess"}]}`, false},
			{"outbound-final", `{"outbounds":[{"type":"vmess","tag":"proxy"}],"route":{"final":"proxy"}}`, false},
		} {
			t.Run(tc.name+"/direct="+strconv.FormatBool(direct), func(t *testing.T) {
				err := checkCapabilities(tc.content, selected)
				if (err != nil) != (tc.implicit && !direct) {
					t.Fatalf("implicit=%v direct=%v: %v", tc.implicit, direct, err)
				}
			})
		}
	}
}

func TestCapabilityGateRejectsMalformedShapes(t *testing.T) {
	selected := map[string][]string{"outbounds": {"vmess", "direct"}, "dns": {"udp"}, "api": {"clash_api"}, "tls": {"utls"}}
	for _, config := range []string{
		`null`, `[]`, `{"inbounds":null}`, `{"outbounds":{}}`, `{"outbounds":[null]}`,
		`{"outbounds":[{"type":5}]}`, `{"dns":[]}`, `{"dns":{"servers":{}}}`,
		`{"experimental":[]}`, `{"experimental":{"clash_api":null}}`,
		`{"outbounds":[{"type":"vmess","transport":"ws"}]}`,
		`{"outbounds":[{"type":"vmess","tls":true}]}`,
		`{"outbounds":[{"type":"vmess","tls":{"enabled":"false"}}]}`,
		`{"outbounds":[{"type":"vmess","tls":{"utls":{"enabled":"false"}}}]}`,
	} {
		if err := checkCapabilities(config, selected); err == nil {
			t.Fatalf("malformed shape accepted: %s", config)
		}
	}
}

func TestCompiledSelectionControlsValidation(t *testing.T) {
	var selected map[string][]string
	if err := json.Unmarshal([]byte(selectedFeaturesJSON), &selected); err != nil {
		t.Fatal(err)
	}
	// Every advertised typed protocol must pass the raw gate; option validation
	// is separate and may require protocol-specific keys.
	for _, category := range []string{"inbounds", "outbounds", "endpoints", "services", "certificate_providers"} {
		for _, name := range selected[category] {
			content, _ := json.Marshal(map[string]any{category: []any{map[string]any{"type": name}}, "route": map[string]any{"final": "fixture"}})
			if err := checkSelectedCapabilities(string(content)); err != nil {
				t.Fatalf("advertised %s rejected: %v", name, err)
			}
		}
	}
	for _, content := range []string{`{"outbounds":[{"type":"not-selected-secret-token"}]}`, `{"services":[{"type":"unknown-secret-token"}]}`} {
		err := CheckConfig(content)
		if err == nil || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("capability error exposed user content: %v", err)
		}
	}
}
