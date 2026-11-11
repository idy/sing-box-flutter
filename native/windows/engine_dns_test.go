// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"

	"github.com/sagernet/sing-box/option"
)

func TestTunDNSPreflightBeforeConstruction(t *testing.T) {
	if !slices.Contains(selectedCapabilities(t)["inbounds"], "tun") {
		t.Skip("test needs tun")
	}
	for _, tc := range []struct {
		name, mode string
		addresses  []string
		servers    []string
		autoRoute  bool
		valid      bool
	}{
		{"ipv4-host-native", "native", []string{"172.19.0.1/32"}, nil, false, false},
		{"ipv4-host-hijack", "hijack", []string{"172.19.0.1/32"}, nil, false, false},
		{"ipv4-host-default-routing", "", []string{"172.19.0.1/32"}, nil, true, false},
		{"ipv6-host-native", "native", []string{"fd00::1/128"}, nil, false, false},
		{"dual-host-native", "native", []string{"172.19.0.1/32", "fd00::1/128"}, nil, false, false},
		{"last-ipv4-address", "native", []string{"172.19.0.3/30"}, nil, false, false},
		{"last-ipv6-address", "native", []string{"fd00::3/126"}, nil, false, false},
		{"missing-addresses", "native", nil, nil, false, false},
		{"invalid-mode", "secret-mode", []string{"172.19.0.1/30"}, nil, false, false},
		{"malformed-address", "native", []string{"secret-prefix"}, nil, false, false},
		{"malformed-explicit-server", "native", []string{"172.19.0.1/32"}, []string{"secret-dns"}, false, false},
		{"unspecified-explicit-server", "native", []string{"172.19.0.1/32"}, []string{"0.0.0.0"}, false, false},
		{"multicast-explicit-server", "native", []string{"fd00::1/128"}, []string{"ff02::1"}, false, false},
		{"zoned-explicit-server", "native", []string{"fd00::1/128"}, []string{"fe80::2%secret-interface"}, false, false},
		{"unmatched-explicit-family", "native", []string{"172.19.0.1/32"}, []string{"fd00::2"}, false, false},
		{"ipv4-derived", "native", []string{"172.19.0.1/30"}, nil, false, true},
		{"ipv6-derived", "native", []string{"fd00::1/126"}, nil, false, true},
		{"dual-derived", "hijack", []string{"172.19.0.1/30", "fd00::1/126"}, nil, true, true},
		{"ipv6-fallback", "native", []string{"172.19.0.1/32", "fd00::1/126"}, nil, false, true},
		{"ipv4-fallback", "native", []string{"172.19.0.1/30", "fd00::1/128"}, nil, false, true},
		{"ipv4-host-explicit", "native", []string{"172.19.0.1/32"}, []string{"172.19.0.2"}, false, true},
		{"ipv6-host-explicit", "native", []string{"fd00::1/128"}, []string{"fd00::2"}, false, true},
		{"host-disabled", "disabled", []string{"172.19.0.1/32"}, nil, true, true},
		{"host-default-manual-routing", "", []string{"172.19.0.1/32"}, nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbound := map[string]any{"type": "tun", "address": tc.addresses, "auto_route": tc.autoRoute}
			if tc.mode != "" {
				inbound["dns_mode"] = tc.mode
			}
			if tc.servers != nil {
				inbound["dns_address"] = tc.servers
			}
			content, err := json.Marshal(map[string]any{"inbounds": []any{inbound}})
			if err != nil {
				t.Fatal(err)
			}
			working := t.TempDir()
			options, err := parseConfig(engineContext(context.Background(), working), string(content), true)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				o := options.Inbounds[0].Options.(*option.TunInboundOptions)
				if o.DNSMode != tc.mode || o.AutoRoute != tc.autoRoute || len(o.DNSAddress) != len(tc.servers) {
					t.Fatal("DNS preflight changed caller choices")
				}
				for index, server := range tc.servers {
					if o.DNSAddress[index].String() != server {
						t.Fatal("DNS preflight changed explicit server")
					}
				}
			} else if err != errConfig {
				t.Fatalf("invalid DNS was accepted or leaked details: %v", err)
			}
			// Check constructs and closes the real Box without opening a Wintun
			// device. Invalid DNS must fail before it can reach native Initialize.
			err = (boxBackend{}).Check(context.Background(), string(content), working)
			if tc.valid && err != nil || !tc.valid && err != errConfig {
				t.Fatalf("unexpected Box check result: %v", err)
			}
		})
	}
}

func TestTunDNSPreflightRejectsInvalidAddressObjects(t *testing.T) {
	for _, options := range []*option.TunInboundOptions{
		{Address: []netip.Prefix{{}}, DNSMode: "native"},
		{Address: []netip.Prefix{netip.MustParsePrefix("172.19.0.1/32")}, DNSMode: "native", DNSAddress: []netip.Addr{{}}},
	} {
		if err := validateTunDNS(options); err != errConfig {
			t.Fatalf("invalid address object accepted: %v", err)
		}
	}
}
