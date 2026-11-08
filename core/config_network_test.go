package core

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
)

func requireIncludedRoutesJSON(t *testing.T, raw []byte, want []string) {
	t.Helper()
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	field := document["included_routes"]
	if len(field) == 0 || string(field) == "null" {
		t.Fatalf("included_routes must be an array, got %s", field)
	}
	var got []string
	if err := json.Unmarshal(field, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("included_routes = %v, want %v", got, want)
	}
	if len(want) == 0 && string(field) != "[]" {
		t.Fatalf("empty included_routes = %s, want []", field)
	}
}

func TestIncludedRouteDefaultsAndExplicitEmptyRoundTrip(t *testing.T) {
	families := []struct {
		name      string
		addresses []string
		defaults  []string
		dns       []string
	}{
		{"IPv4", []string{"10.7.0.2/32"}, []string{"0.0.0.0/0"}, []string{"198.18.0.2"}},
		{"IPv6", []string{"fd00:7::2/128"}, []string{"::/0"}, []string{"2001:2::2"}},
		{"dual", []string{"fd00:7::2/128", "10.7.0.2/32"}, []string{"0.0.0.0/0", "::/0"}, []string{"198.18.0.2"}},
	}
	for _, family := range families {
		for _, mode := range []string{"network omitted", "routes omitted", "routes null", "routes empty"} {
			t.Run(family.name+"/"+mode, func(t *testing.T) {
				config := testConfig(t)
				config.Addresses = family.addresses
				raw, err := json.Marshal(config)
				if err != nil {
					t.Fatal(err)
				}
				var document map[string]any
				if err := json.Unmarshal(raw, &document); err != nil {
					t.Fatal(err)
				}
				delete(document, "network")
				if mode != "network omitted" {
					network := map[string]any{"excluded_routes": []string{}}
					if mode == "routes null" {
						network["included_routes"] = nil
					} else if mode == "routes empty" {
						network["included_routes"] = []string{}
					}
					document["network"] = network
				}
				raw, err = json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				config, err = DecodeConfig(raw)
				if err != nil {
					t.Fatal(err)
				}
				want := family.defaults
				if mode == "routes empty" {
					want = []string{}
				}
				for round := range 3 {
					config, err = NormalizeConfig(config)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(config.Network.IncludedRoutes, want) {
						t.Fatalf("round %d routes = %v, want %v", round, config.Network.IncludedRoutes, want)
					}
					raw, err = json.Marshal(config)
					if err != nil {
						t.Fatal(err)
					}
					var encoded struct {
						Network json.RawMessage `json:"network"`
					}
					if err := json.Unmarshal(raw, &encoded); err != nil {
						t.Fatal(err)
					}
					requireIncludedRoutesJSON(t, encoded.Network, want)
					config, err = DecodeConfig(raw)
					if err != nil {
						t.Fatal(err)
					}
				}
				settings, err := Validate(config)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(settings.Addresses, family.addresses) || !reflect.DeepEqual(settings.DNSServers, family.dns) {
					t.Fatalf("unexpected derived addresses or DNS: %+v", settings)
				}
				raw, err = json.Marshal(settings)
				if err != nil {
					t.Fatal(err)
				}
				requireIncludedRoutesJSON(t, raw, want)
			})
		}
	}
}

func testNetworkRoutes(count int) []string {
	routes := make([]string, count)
	for index := range routes {
		routes[index] = netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 42, byte(index >> 8), byte(index)}), 32).String()
	}
	return routes
}

func TestNetworkRouteInputLimits(t *testing.T) {
	for _, field := range []string{"included", "excluded", "both"} {
		for _, count := range []int{1024, 1025} {
			t.Run(fmt.Sprintf("%s/%d", field, count), func(t *testing.T) {
				config := testConfig(t)
				if field != "excluded" {
					config.Network.IncludedRoutes = testNetworkRoutes(count)
				}
				if field != "included" {
					config.Network.ExcludedRoutes = testNetworkRoutes(count)
				}
				_, normalizedErr := NormalizeConfig(config)
				settings, err := Validate(config)
				if count == 1025 {
					if err == nil || normalizedErr == nil {
						t.Fatal("1025 input routes accepted")
					}
					return
				}
				if err != nil || normalizedErr != nil {
					t.Fatalf("1024 input routes rejected: normalize=%v, validate=%v", normalizedErr, err)
				}
				if field != "excluded" && len(settings.IncludedRoutes) != count {
					t.Fatalf("included routes = %d, want %d", len(settings.IncludedRoutes), count)
				}
				if field != "included" && len(settings.ExcludedRoutes) != count+1 {
					t.Fatalf("derived excluded routes = %d, want %d", len(settings.ExcludedRoutes), count+1)
				}
			})
		}
	}
}

func TestDerivedExclusionLimitAndEndpointDeduplication(t *testing.T) {
	for _, mode := range []string{"32 unique endpoints", "existing IPv6 alias", "same hosts different ports", "IPv6 endpoint aliases", "mapped IPv4 endpoint"} {
		t.Run(mode, func(t *testing.T) {
			config := testConfig(t)
			config.Network.ExcludedRoutes = testNetworkRoutes(1024)
			config.Network.IncludedRoutes = testNetworkRoutes(1024)
			config.Peers = make([]Peer, 32)
			wantCount := 1056
			for index := range config.Peers {
				_, public := testKeys(t)
				endpoint := fmt.Sprintf("203.0.113.%d:51820", index+1)
				switch mode {
				case "existing IPv6 alias":
					if index == 0 {
						config.Network.ExcludedRoutes[0] = "2001:0DB8:0000:0000:0000:0000:0000:0001/128"
						endpoint = "[2001:db8::1]:51820"
					}
					wantCount = 1055
				case "same hosts different ports":
					endpoint = fmt.Sprintf("203.0.113.%d:%d", index/2+1, 51820+index%2)
					wantCount = 1040
				case "IPv6 endpoint aliases":
					endpoint = "[2001:db8::1]:51820"
					if index%2 == 0 {
						endpoint = "[2001:0DB8:0000:0000:0000:0000:0000:0001]:51821"
					}
					wantCount = 1025
				case "mapped IPv4 endpoint":
					endpoint = "203.0.113.1:51820"
					if index%2 == 0 {
						endpoint = "[::ffff:203.0.113.1]:51821"
					}
					wantCount = 1025
				}
				config.Peers[index] = Peer{ID: fmt.Sprintf("peer%d", index), PublicKey: public, Endpoint: endpoint}
			}
			settings, err := Validate(config)
			if err != nil {
				t.Fatal(err)
			}
			if len(settings.ExcludedRoutes) != wantCount {
				t.Fatalf("derived exclusions = %d, want %d", len(settings.ExcludedRoutes), wantCount)
			}
			if !reflect.DeepEqual(settings.ExcludedRoutes[:1024], config.Network.ExcludedRoutes) {
				t.Fatal("caller exclusions changed during derivation")
			}
			seen := make(map[netip.Prefix]bool)
			for _, route := range settings.ExcludedRoutes {
				prefix := netip.MustParsePrefix(route)
				if seen[prefix] {
					t.Fatalf("duplicate endpoint route %s", route)
				}
				seen[prefix] = true
			}
			raw, err := json.Marshal(settings)
			if err != nil {
				t.Fatal(err)
			}
			var decoded NetworkSettings
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.ExcludedRoutes) > 1056 || len(decoded.IncludedRoutes) != 1024 {
				t.Fatalf("final ABI route counts: included=%d excluded=%d", len(decoded.IncludedRoutes), len(decoded.ExcludedRoutes))
			}
		})
	}
}

func TestTunnelDNSCountLimits(t *testing.T) {
	for _, count := range []int{0, 8, 9} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			config := testConfig(t)
			config.Network.DNSServers = make([]string, count)
			for index := range config.Network.DNSServers {
				config.Network.DNSServers[index] = fmt.Sprintf("198.18.0.%d", index+1)
			}
			settings, err := Validate(config)
			if count != 8 {
				if err == nil {
					t.Fatalf("%d explicit DNS servers accepted", count)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(settings.DNSServers, config.Network.DNSServers) {
				t.Fatalf("DNS servers changed: %v", settings.DNSServers)
			}
		})
	}
}

func TestNetworkSettingsAddressFamilyValidation(t *testing.T) {
	for _, test := range []struct {
		name      string
		addresses []string
		dns       []string
		included  []string
		excluded  []string
		valid     bool
	}{
		{"IPv4 rejects IPv6 included", []string{"10.7.0.2/32"}, nil, []string{"::/0"}, nil, false},
		{"IPv6 rejects IPv4 included", []string{"fd00:7::2/128"}, nil, []string{"0.0.0.0/0"}, nil, false},
		{"IPv4 rejects IPv6 DNS", []string{"10.7.0.2/32"}, []string{"fd00:7::53"}, nil, nil, false},
		{"IPv6 rejects IPv4 DNS", []string{"fd00:7::2/128"}, []string{"198.18.0.2"}, nil, nil, false},
		{"mapped DNS rejected", []string{"10.7.0.2/32", "fd00:7::2/128"}, []string{"::ffff:198.18.0.2"}, nil, nil, false},
		{"IPv4 allows outer IPv6 exclusion", []string{"10.7.0.2/32"}, nil, nil, []string{"2001:db8::1/128"}, true},
		{"IPv6 allows outer IPv4 exclusion", []string{"fd00:7::2/128"}, nil, nil, []string{"203.0.113.1/32"}, true},
		{"dual allows both DNS and route families", []string{"10.7.0.2/32", "fd00:7::2/128"}, []string{"198.18.0.2", "fd00:7::53"}, []string{"0.0.0.0/0", "::/0"}, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t)
			config.Addresses = test.addresses
			config.Network = NetworkSettings{DNSServers: test.dns, IncludedRoutes: test.included, ExcludedRoutes: test.excluded}
			settings, err := Validate(config)
			if (err == nil) != test.valid {
				t.Fatalf("Validate error = %v, want valid=%v", err, test.valid)
			}
			if test.valid && len(test.excluded) > 0 && settings.ExcludedRoutes[0] != test.excluded[0] {
				t.Fatal("outer-family exclusion was lost")
			}
		})
	}
}
