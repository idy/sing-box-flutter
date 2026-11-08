package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/idy/sing-box-flutter/core/transport"
	wgc "github.com/idy/wgc"
)

func TestLocalAddressUniquenessBeforeTransportConstruction(t *testing.T) {
	for _, test := range []struct {
		name      string
		addresses []string
		valid     bool
	}{
		{"IPv4 exact duplicate", []string{"10.7.0.2/32", "10.7.0.2/32"}, false},
		{"IPv4 same host different prefix", []string{"10.7.0.2/24", "10.7.0.2/32"}, false},
		{"IPv6 exact duplicate", []string{"fd00:7::2/128", "fd00:7::2/128"}, false},
		{"IPv6 aliases", []string{"fd00:7::2/64", "FD00:0007:0000:0000:0000:0000:0000:0002/64"}, false},
		{"IPv6 aliases and different prefixes", []string{"fd00:7::2/64", "FD00:0007:0000:0000:0000:0000:0000:0002/128"}, false},
		{"IPv4 distinct hosts same subnet", []string{"10.7.0.2/24", "10.7.0.3/24"}, true},
		{"IPv6 distinct hosts same subnet", []string{"fd00:7::2/64", "FD00:0007:0000:0000:0000:0000:0000:0003/64"}, true},
		{"distinct dual hosts", []string{"10.7.0.2/32", "fd00:7::2/128"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t)
			config.Addresses = test.addresses
			normalized, err := NormalizeConfig(config)
			if (err == nil) != test.valid {
				t.Fatalf("NormalizeConfig error = %v, want valid=%v", err, test.valid)
			}
			if test.valid && !reflect.DeepEqual(normalized.Addresses, test.addresses) {
				t.Fatalf("interface host bits or spelling changed: %v", normalized.Addresses)
			}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeConfig(raw); (err == nil) != test.valid {
				t.Fatalf("DecodeConfig error = %v, want valid=%v", err, test.valid)
			}
			if _, err := Validate(config); (err == nil) != test.valid {
				t.Fatalf("Validate error = %v, want valid=%v", err, test.valid)
			}

			// Stop successful validation at the transport boundary so no peer
			// device (including the upstream duplicate-address failure) is built.
			registry := transport.NewRegistry()
			opened := false
			stop := errors.New("test transport construction boundary")
			if err := registry.RegisterFactory("config-address-test", func(context.Context, json.RawMessage) (wgc.Transport, error) {
				opened = true
				return nil, stop
			}); err != nil {
				t.Fatal(err)
			}
			config.Transport.Type = "config-address-test"
			session, err := NewSession(context.Background(), config, SessionOptions{Registry: registry})
			if session != nil {
				_ = session.Close()
				t.Fatal("unexpected session past transport boundary")
			}
			if test.valid {
				if !opened || !errors.Is(err, stop) {
					t.Fatalf("distinct addresses did not reach transport construction: opened=%v error=%v", opened, err)
				}
			} else if opened || err == nil {
				t.Fatalf("duplicate host reached transport construction: opened=%v error=%v", opened, err)
			}
		})
	}
}
