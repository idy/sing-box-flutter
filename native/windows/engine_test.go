// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
)

func selectedCapabilities(t *testing.T) map[string][]string {
	t.Helper()
	var selected map[string][]string
	if err := json.Unmarshal([]byte(selectedFeaturesJSON), &selected); err != nil {
		t.Fatal(err)
	}
	return selected
}

func TestCapabilityGateRejectsUnselectedSharedImplementations(t *testing.T) {
	for _, options := range []any{
		&option.VMessOutboundOptions{Transport: &option.V2RayTransportOptions{Type: "ws"}},
		&option.OutboundTLSOptions{UTLS: &option.OutboundUTLSOptions{Enabled: true}},
		&option.InboundTLSOptions{Reality: &option.InboundRealityOptions{Enabled: true}},
		&option.OutboundTLSOptions{Reality: &option.OutboundRealityOptions{Enabled: true}},
		&option.InboundTLSOptions{ECH: &option.InboundECHOptions{Enabled: true}},
		&option.OutboundTLSOptions{ECH: &option.OutboundECHOptions{Enabled: true}},
		&option.ExperimentalOptions{ClashAPI: &option.ClashAPIOptions{}},
		&option.ExperimentalOptions{V2RayAPI: &option.V2RayAPIOptions{}},
	} {
		// The decoder stores registry options behind interfaces and pointers.
		wrapped := option.Options{Outbounds: []option.Outbound{{Type: "fixture", Options: options}}}
		if err := checkCapabilities(wrapped, nil); err == nil {
			t.Fatal("accepted an unselected shared implementation")
		}
		selected := map[string][]string{"transports": {"ws"}, "tls": {"utls", "reality", "ech"}, "api": {"clash_api", "v2ray_api"}}
		if err := checkCapabilities(wrapped, selected); err != nil {
			t.Fatal(err)
		}
	}
	opaque := struct {
		Transport        string
		TransportOptions map[string]any
	}{Transport: "application", TransportOptions: map[string]any{"tls": map[string]any{"utls": map[string]any{"enabled": true}}, "transport": "private"}}
	if err := checkCapabilities(opaque, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityGateRequiresImplicitDirect(t *testing.T) {
	if checkCapabilities(option.Options{}, nil) == nil {
		t.Fatal("unselected implicit direct accepted")
	}
	options := option.Options{Endpoints: []option.Endpoint{{Type: "wgc", Tag: "vpn"}}, Route: &option.RouteOptions{Final: "vpn"}}
	if err := checkCapabilities(options, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedRegistryValidationAndSanitizedErrors(t *testing.T) {
	working := t.TempDir()
	for _, content := range []string{
		`{"outbounds":[{"type":"secret-unselected-protocol","password":"secret-password"}]}`,
		`{"inbounds":[{"type":"secret-unselected-protocol"}]}`,
		`{"dns":{"servers":[{"type":"secret-unselected-dns"}]}}`,
		`{"services":[{"type":"secret-unselected-service"}]}`,
		`{"certificate_providers":[{"type":"secret-unselected-provider"}]}`,
		`null`, `[]`, `{"outbounds":{}}`, `{"outbounds":[null]}`, `{"outbounds":[{}]}`, `secret-invalid-json`,
		`{"dns":[]}`, `{"experimental":[]}`,
		`{"outbounds":[{"type":"vmess","transport":"ws"}]}`,
		`{"outbounds":[{"type":"vmess","tls":true}]}`,
		`{"outbounds":[{"type":"vmess","tls":{"utls":{"enabled":"true"}}}]}`,
	} {
		err := (boxBackend{}).Check(context.Background(), content, working)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid config accepted or leaked: %v", err)
		}
	}
	ctx := engineContext(context.Background(), working)
	if service.FromContext[adapter.PlatformInterface](ctx) != nil {
		t.Fatal("Windows must use native TUN and interface monitoring")
	}
}

func TestNativeTunConfigurationPreservesExplicitRoutingAndPacketStack(t *testing.T) {
	selected := selectedCapabilities(t)
	if !slices.Contains(selected["inbounds"], "tun") || !slices.Contains(selected["outbounds"], "direct") {
		t.Skip("test needs tun and direct")
	}
	for _, stack := range []string{"", "system", "gvisor", "mixed"} {
		content := `{"inbounds":[{"type":"tun","address":["172.19.0.1/30"],"stack":"` + stack + `","auto_route":false}],"outbounds":[{"type":"direct"}],"route":{"auto_detect_interface":false},"log":{"output":"secret-log-file"},"experimental":{"debug":{"listen":"127.0.0.1:6060"}}}`
		options, err := parseConfig(engineContext(context.Background(), t.TempDir()), content, true)
		if err != nil {
			t.Fatalf("stack %s rejected: %v", stack, err)
		}
		tun := options.Inbounds[0].Options.(*option.TunInboundOptions)
		wantStack := stack
		if wantStack == "" {
			wantStack = "system"
		}
		if tun.Stack != wantStack || tun.AutoRoute {
			t.Fatal("explicit stack/routing changed")
		}
		if options.Route == nil || options.Route.AutoDetectInterface {
			t.Fatal("explicit interface detection setting changed")
		}
		if options.Log == nil || !options.Log.Disabled || options.Log.Output != "" || options.Experimental.Debug != nil {
			t.Fatal("raw diagnostics enabled")
		}
	}
	// checkConfig is useful for generic configs, while start must own a TUN.
	content := `{"outbounds":[{"type":"direct"}]}`
	if err := (boxBackend{}).Check(context.Background(), content, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := (boxBackend{}).New(context.Background(), content, t.TempDir()); err == nil {
		t.Fatal("start accepted configuration without TUN")
	}
}

func TestEffectiveOptionsGateTransportTLSAndNativeSyntax(t *testing.T) {
	selected := selectedCapabilities(t)
	if !slices.Contains(selected["outbounds"], "direct") {
		t.Skip("test needs direct")
	}
	ctx := engineContext(context.Background(), t.TempDir())
	if _, err := parseConfig(ctx, "{\"outbounds\":[{\"type\":\"direct\", /* comment */ \"tag\":\"d\"},], // comment\n\"route\":{}}", false); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig(ctx, `{"inbounds":null,"outbounds":null,"experimental":{"clash_api":null}}`, false); err != nil {
		t.Fatalf("valid native null optional objects rejected: %v", err)
	}
	if !slices.Contains(selected["outbounds"], "vmess") {
		return
	}
	for _, content := range []string{
		`{"outbounds":[{"type":"vmess","server":"192.0.2.1","server_port":443,"uuid":"00000000-0000-0000-0000-000000000000","transport":{"type":"secret-unselected-transport"}}]}`,
		`{"outbounds":[{"type":"vmess","tls":{"utls":{"enabled":true}}}]}`,
		`{"outbounds":[{"type":"vmess","TLS":{"UTLS":{"enabled":true}}}]}`,
	} {
		if strings.Contains(content, "utls") || strings.Contains(content, "UTLS") {
			if slices.Contains(selected["tls"], "utls") {
				continue
			}
		}
		if _, err := parseConfig(ctx, content, false); err == nil {
			t.Fatal("effective unselected capability accepted")
		}
	}
}

func TestFailedConstructorReturnsNilCoreAndProtocolError(t *testing.T) {
	instance, err := (boxBackend{}).New(context.Background(), `{"outbounds":[{"type":"secret-unselected-protocol"}]}`, t.TempDir())
	if err == nil || instance != nil {
		t.Fatal("failed constructor returned a boxed nil core")
	}
	h := newHarness(t, boxBackend{})
	h.initialize()
	h.send(2, "start", map[string]any{"config": `{"outbounds":[{"type":"secret-unselected-protocol"}]}`})
	h.response(2, "")
	h.event("connecting")
	h.event("error")
	h.send(3, "getStatus", nil)
	h.response(3, "")
}

func TestOperationContextPropagatesCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx := engineContext(parent, t.TempDir())
	cancel()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("engine lost operation cancellation")
	}
	if ctx.Err() == nil || service.FromContext[adapter.InboundRegistry](ctx) == nil {
		t.Fatal("missing cancellation or generated registry")
	}
}
