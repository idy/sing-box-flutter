// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/option"
	nativejson "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

func TestCapabilityGatesSharedImplementations(t *testing.T) {
	for _, tc := range []struct {
		name, category, feature string
		options                 any
	}{
		{"websocket", "transports", "ws", &option.VMessOutboundOptions{Transport: &option.V2RayTransportOptions{Type: "ws"}}},
		{"grpc", "transports", "grpc", &option.VLESSOutboundOptions{Transport: &option.V2RayTransportOptions{Type: "grpc"}}},
		{"http", "transports", "http", &option.TrojanInboundOptions{Transport: &option.V2RayTransportOptions{Type: "http"}}},
		{"quic", "transports", "quic", &option.TrojanOutboundOptions{Transport: &option.V2RayTransportOptions{Type: "quic"}}},
		{"httpupgrade", "transports", "httpupgrade", &option.VMessInboundOptions{Transport: &option.V2RayTransportOptions{Type: "httpupgrade"}}},
		{"utls", "tls", "utls", &option.VMessOutboundOptions{OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{TLS: &option.OutboundTLSOptions{UTLS: &option.OutboundUTLSOptions{Enabled: true}}}}},
		{"inbound-reality", "tls", "reality", &option.TrojanInboundOptions{InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{TLS: &option.InboundTLSOptions{Reality: &option.InboundRealityOptions{Enabled: true}}}}},
		{"outbound-reality", "tls", "reality", &option.RemoteTLSDNSServerOptions{OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{TLS: &option.OutboundTLSOptions{Reality: &option.OutboundRealityOptions{Enabled: true}}}}},
		{"inbound-ech", "tls", "ech", &option.APIServiceOptions{InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{TLS: &option.InboundTLSOptions{ECH: &option.InboundECHOptions{Enabled: true}}}}},
		{"outbound-ech", "tls", "ech", &option.HTTPClient{OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{TLS: &option.OutboundTLSOptions{ECH: &option.OutboundECHOptions{Enabled: true}}}}},
		{"clash-api", "api", "clash_api", &option.ExperimentalOptions{ClashAPI: &option.ClashAPIOptions{}}},
		{"v2ray-api", "api", "v2ray_api", &option.ExperimentalOptions{V2RayAPI: &option.V2RayAPIOptions{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Registry payloads are interfaces in json:"-" fields, not raw maps.
			options := &option.Options{Outbounds: []option.Outbound{{Type: "fixture", Options: tc.options}}}
			selected := map[string][]string{}
			if err := checkCapabilities(options, selected); err == nil {
				t.Fatal("unselected shared implementation accepted")
			}
			selected[tc.category] = []string{tc.feature}
			if err := checkCapabilities(options, selected); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCapabilityGateAllowsDisabledTLSFeatures(t *testing.T) {
	options := []any{
		&option.InboundTLSOptions{ECH: &option.InboundECHOptions{}, Reality: &option.InboundRealityOptions{}},
		&option.OutboundTLSOptions{UTLS: &option.OutboundUTLSOptions{}, ECH: &option.OutboundECHOptions{}, Reality: &option.OutboundRealityOptions{}},
	}
	if err := checkCapabilities(options, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityGateRequiresSelectedImplicitDirect(t *testing.T) {
	for _, direct := range []bool{false, true} {
		selected := map[string][]string{}
		if direct {
			selected["outbounds"] = []string{"direct"}
		}
		for _, tc := range []struct {
			name     string
			options  option.Options
			implicit bool
		}{
			{"empty", option.Options{}, true},
			{"empty-outbounds", option.Options{Outbounds: []option.Outbound{}}, true},
			{"tun-dns-only", option.Options{Inbounds: []option.Inbound{{Type: "tun"}}, DNS: &option.DNSOptions{}}, true},
			{"endpoint-without-final", option.Options{Endpoints: []option.Endpoint{{Type: "wgc", Tag: "vpn"}}}, true},
			{"endpoint-empty-final", option.Options{Endpoints: []option.Endpoint{{Type: "wgc", Tag: "vpn"}}, Route: &option.RouteOptions{}}, true},
			{"endpoint-final", option.Options{Endpoints: []option.Endpoint{{Type: "wgc", Tag: "vpn"}}, Route: &option.RouteOptions{Final: "vpn"}}, false},
			{"first-outbound", option.Options{Outbounds: []option.Outbound{{Type: "vmess"}}}, false},
			{"outbound-final", option.Options{Outbounds: []option.Outbound{{Type: "vmess", Tag: "proxy"}}, Route: &option.RouteOptions{Final: "proxy"}}, false},
		} {
			t.Run(tc.name+"/direct="+strconv.FormatBool(direct), func(t *testing.T) {
				err := checkCapabilities(tc.options, selected)
				if (err != nil) != (tc.implicit && !direct) {
					t.Fatalf("implicit=%v direct=%v: %v", tc.implicit, direct, err)
				}
			})
		}
	}
}

type capabilityOptionsRegistry map[string]func() any

func (r capabilityOptionsRegistry) OptionTypes() []string {
	var names []string
	for name := range r {
		names = append(names, name)
	}
	return names
}

func (r capabilityOptionsRegistry) CreateOptions(name string) (any, bool) {
	factory, ok := r[name]
	if !ok {
		return nil, false
	}
	return factory(), true
}

type capabilityOpaqueEndpointOptions struct {
	Transport        string                `json:"transport,omitempty"`
	TransportOptions nativejson.RawMessage `json:"transport_options,omitempty"`
}

// Install only option factories, including a stand-in for the application-owned
// WGC schema. Capability tests must also compile in profiles without WGC's module.
func capabilityDecodeContext() context.Context {
	ctx := context.Background()
	ctx = service.ContextWith[option.InboundOptionsRegistry](ctx, capabilityOptionsRegistry{
		"trojan": func() any { return new(option.TrojanInboundOptions) },
	})
	ctx = service.ContextWith[option.OutboundOptionsRegistry](ctx, capabilityOptionsRegistry{
		"direct": func() any { return new(option.DirectOutboundOptions) },
		"vmess":  func() any { return new(option.VMessOutboundOptions) },
	})
	ctx = service.ContextWith[option.EndpointOptionsRegistry](ctx, capabilityOptionsRegistry{
		"wgc": func() any { return new(capabilityOpaqueEndpointOptions) },
	})
	ctx = service.ContextWith[option.DNSTransportOptionsRegistry](ctx, capabilityOptionsRegistry{
		"udp": func() any { return new(option.RemoteDNSServerOptions) },
	})
	ctx = service.ContextWith[option.ServiceOptionsRegistry](ctx, capabilityOptionsRegistry{})
	return service.ContextWith[option.CertificateProviderOptionsRegistry](ctx, capabilityOptionsRegistry{})
}

func decodeCapabilityOptions(content string) (*option.Options, error) {
	return nativejson.UnmarshalExtendedContext[*option.Options](capabilityDecodeContext(), []byte(content))
}

func TestCapabilityGateUsesNativeDecoding(t *testing.T) {
	for _, content := range []string{
		`[]`, `{"outbounds":{}}`, `{"outbounds":[null]}`, `{"outbounds":[{}]}`,
		`{"outbounds":[{"type":5}]}`, `{"dns":[]}`,
		`{"experimental":[]}`, `{"outbounds":[{"type":"vmess","transport":"ws"}]}`,
		`{"outbounds":[{"type":"vmess","tls":true}]}`,
		`{"outbounds":[{"type":"vmess","tls":{"enabled":"false"}}]}`,
		`{"outbounds":[{"type":"vmess","tls":{"utls":{"enabled":"false"}}}]}`,
		`{"services":[{"type":"api"}]}`, `{"certificate_providers":[{"type":"acme"}]}`,
		`{"inbounds":[{"type":"trojan","tls":{"certificate_provider":{"type":"acme"}}}]}`,
	} {
		if _, err := decodeCapabilityOptions(content); err == nil {
			t.Errorf("native decoding accepted malformed or unregistered options: %s", content)
		}
	}
	if options, err := decodeCapabilityOptions(`null`); err != nil || options != nil {
		t.Fatalf("native null semantics changed: options=%v err=%v", options, err)
	}
	// Null optional objects/lists and native comments are valid decoder syntax.
	for _, content := range []string{
		`{"inbounds":null,"outbounds":null,"experimental":{"clash_api":null}}`,
		`{"dns":{"servers":{}}}`,                          // The pinned decoder treats this as an empty server list.
		`{"outbounds":[{"type":"vmess","transport":{}}]}`, // Empty optional transport decodes to nil.
		"{\"outbounds\":[{\"type\":\"direct\", /* comment */ \"tag\":\"d\"},], // comment\n\"route\":{}}",
	} {
		options, err := decodeCapabilityOptions(content)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkCapabilities(*options, map[string][]string{"outbounds": {"direct"}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCapabilityGateUsesEffectiveAliasesAndRepeatedFields(t *testing.T) {
	for _, tc := range []struct {
		content string
		blocked bool
	}{
		{`{"outbounds":[{"type":"vmess","Transport":{"type":"ws"}}]}`, true},
		{`{"outbounds":[{"type":"vmess","TLS":{"UTLS":{"enabled":true}}}]}`, true},
		{`{"http_clients":[{"tag":"client","version":1,"tls":{"ech":{"enabled":true},"ech":{}}}],"route":{"final":"fixture"}}`, true},
		{`{"HTTP_Clients":[{"tag":"client","version":1,"TLS":{"ECH":{"enabled":true},"ech":{"enabled":false}}}],"route":{"final":"fixture"}}`, false},
		{`{"outbounds":[{"type":"vmess","tls":{"utls":{"enabled":true},"UTLS":{"enabled":false}}}]}`, false},
		{`{"experimental":{"CLASH_API":{}},"route":{"final":"fixture"}}`, true},
	} {
		options, err := decodeCapabilityOptions(tc.content)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkCapabilities(*options, nil); (err != nil) != tc.blocked {
			t.Fatalf("blocked=%v: %v", tc.blocked, err)
		}
	}
}

func TestCapabilityGatePreservesOpaqueData(t *testing.T) {
	content := `{"endpoints":[{"type":"wgc","tag":"vpn","transport":"app","transport_options":{"tls":{"utls":{"enabled":true}},"Transport":{"type":"secret-application-protocol"},"certificate_provider":{"type":"secret-provider"}}}],"http_clients":[{"tag":"client","version":1,"headers":{"tls":"opaque","transport":"application","certificate_provider":"private","utls":"private"}}],"route":{"final":"vpn"}}`
	options, err := decodeCapabilityOptions(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkCapabilities(*options, nil); err != nil {
		t.Fatal(err)
	}
	opaque := options.Endpoints[0].Options.(*capabilityOpaqueEndpointOptions)
	if opaque.Transport != "app" || !strings.Contains(string(opaque.TransportOptions), "secret-application-protocol") || options.HTTPClients[0].Headers["tls"][0] != "opaque" {
		t.Fatal("capability validation changed application data")
	}
	// Map keys and similarly named struct fields have no capability semantics.
	type userOptions struct {
		TLS       map[string]any
		Transport string
	}
	if err := checkCapabilities(userOptions{TLS: map[string]any{"utls": map[string]any{"enabled": true}}, Transport: "private"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityGateHandlesCyclesAndSharedSlices(t *testing.T) {
	type wrapper struct {
		Next    *wrapper
		Options any
	}
	cycle := &wrapper{}
	cycle.Next = cycle
	cycle.Options = map[string]any{"cycle": cycle}
	if err := checkCapabilities(cycle, nil); err != nil {
		t.Fatal(err)
	}
	cycle.Options.(map[string]any)["tls"] = &option.OutboundUTLSOptions{Enabled: true}
	if err := checkCapabilities(cycle, nil); err == nil {
		t.Fatal("cycle hid a real TLS capability")
	}
	values := []any{nil, &option.OutboundUTLSOptions{Enabled: true}}
	if err := checkCapabilities([]any{values[:1], values}, nil); err == nil {
		t.Fatal("short slice hid a shared longer slice's capability")
	}
	self := make([]any, 1)
	self[0] = self
	if err := checkCapabilities(self, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityGateScalarAndCompositeContainers(t *testing.T) {
	type label string
	for _, value := range []any{
		[]label{"tls", "transport"}, [2]label{"utls", "reality"},
		map[string]label{"tls": "utls"}, []byte{1, 2, 3},
	} {
		if err := checkCapabilities(value, nil); err != nil {
			t.Fatalf("scalar-only container requested a capability: %v", err)
		}
	}
	for _, value := range []any{
		[]option.OutboundUTLSOptions{{Enabled: true}},
		[1]option.OutboundUTLSOptions{{Enabled: true}},
		map[string]option.OutboundUTLSOptions{"tls": {Enabled: true}},
		[]any{&option.OutboundUTLSOptions{Enabled: true}},
		[1]*option.OutboundUTLSOptions{{Enabled: true}},
		map[string]any{"tls": &option.OutboundUTLSOptions{Enabled: true}},
	} {
		if err := checkCapabilities(value, nil); err == nil {
			t.Fatalf("composite container hid a real TLS capability: %T", value)
		}
		if err := checkCapabilities(value, map[string][]string{"tls": {"utls"}}); err != nil {
			t.Fatalf("selected TLS capability rejected: %v", err)
		}
	}
}

func BenchmarkCapabilityScalarCollections(b *testing.B) {
	type label string
	for _, count := range []int{0, 100000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			options := &struct {
				Domains []label
				Ports   map[string]uint16
			}{Domains: make([]label, count), Ports: make(map[string]uint16, count)}
			for index := 0; index < count; index++ {
				options.Domains[index] = "fixture.example"
				options.Ports[strconv.Itoa(index)] = 443
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if err := checkCapabilities(options, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestCompiledCapabilitiesDecodeErrorsAreStatic(t *testing.T) {
	for _, content := range []string{`null`, `[]`, `{`, `{"tls":true}`, `{"tls":["private",7]}`} {
		if _, err := decodeCapabilities(content); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("invalid compiled selection accepted or exposed: %v", err)
		}
	}
}

func TestCompiledSelectionControlsValidation(t *testing.T) {
	var selected map[string][]string
	if err := json.Unmarshal([]byte(selectedFeaturesJSON), &selected); err != nil {
		t.Fatal(err)
	}
	ctx := mobileBaseContext(nil)
	for category, registry := range map[string]interface{ OptionTypes() []string }{
		"inbounds":              service.FromContext[option.InboundOptionsRegistry](ctx),
		"outbounds":             service.FromContext[option.OutboundOptionsRegistry](ctx),
		"endpoints":             service.FromContext[option.EndpointOptionsRegistry](ctx),
		"dns":                   service.FromContext[option.DNSTransportOptionsRegistry](ctx),
		"services":              service.FromContext[option.ServiceOptionsRegistry](ctx),
		"certificate_providers": service.FromContext[option.CertificateProviderOptionsRegistry](ctx),
	} {
		got, want := registry.OptionTypes(), slices.Clone(selected[category])
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("%s constructor selection differs from the compiled capabilities", category)
		}
	}
	for _, content := range []string{`{"outbounds":[{"type":"not-selected-secret-token"}]}`, `{"services":[{"type":"unknown-secret-token"}]}`} {
		err := CheckConfig(content)
		if err == nil || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("capability error exposed user content: %v", err)
		}
	}
	options := option.Options{Route: &option.RouteOptions{Final: "fixture"}, HTTPClients: []option.HTTPClient{{OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{TLS: &option.OutboundTLSOptions{UTLS: &option.OutboundUTLSOptions{Enabled: true}}}}}}
	if err := checkSelectedCapabilities(options); (err == nil) != slices.Contains(selected["tls"], "utls") {
		t.Fatalf("compiled TLS selection was not used: %v", err)
	}
}
