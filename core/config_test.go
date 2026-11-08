package core

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func testKeys(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key.Bytes()), base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())
}
func testConfig(t *testing.T) Config {
	t.Helper()
	private, _ := testKeys(t)
	_, public := testKeys(t)
	return Config{Version: 1, PrivateKey: private, Addresses: []string{"10.7.0.2/32"}, Peers: []Peer{{ID: "exit", PublicKey: public, Endpoint: "127.0.0.1:9"}}}
}

func TestConfigurationValidationAndPublicNetworkSettings(t *testing.T) {
	config := testConfig(t)
	raw, _ := json.Marshal(config)
	parsed, err := DecodeConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := Validate(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if settings.MTU != 1280 || len(settings.ExcludedRoutes) != 1 || settings.ExcludedRoutes[0] != "127.0.0.1/32" {
		t.Fatal(settings)
	}
	public, _ := json.Marshal(settings)
	var document map[string]any
	if err := json.Unmarshal(public, &document); err != nil {
		t.Fatal(err)
	}
	if _, found := document["private_key"]; found {
		t.Fatal("secret in public settings")
	}
}
func TestInvalidConfiguration(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.Version = 2 }, func(c *Config) { c.PrivateKey = "bad" }, func(c *Config) { c.Addresses = nil },
		func(c *Config) { c.Peers[0].Endpoint = "example.com:51820" }, func(c *Config) { c.Peers[0].Endpoint = "127.0.0.1:0" },
		func(c *Config) { c.Peers[0].PublicKey = base64.StdEncoding.EncodeToString(make([]byte, 32)) },
		func(c *Config) { c.MTU = 9000 }, func(c *Config) { c.DefaultPeer = "missing" }, func(c *Config) { c.DNSServer = "dns.example:53" },
		func(c *Config) { c.Network.IncludedRoutes = []string{"10.0.0.1/8"} },
	} {
		config := testConfig(t)
		change(&config)
		if _, err := NormalizeConfig(config); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"version":1,"unexpected":true}`, `{} {}`} {
		if _, err := DecodeConfig([]byte(raw)); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
}
func TestNestedSingBoxRejected(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"inbounds":[{"auto_route":true}]}`, `{"outbounds":[{"peer":"undeclared"}]}`} {
		config := testConfig(t)
		config.SingBox = json.RawMessage(raw)
		if _, err := NormalizeConfig(config); err == nil {
			t.Fatal("nested sing-box configuration accepted")
		}
		if _, err := Validate(config); err == nil {
			t.Fatal("nested sing-box configuration passed validation")
		}
	}
}
