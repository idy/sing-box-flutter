// Package core implements the WGC sing-box endpoint without an OS TUN runtime.
package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"regexp"
	"slices"

	"github.com/idy/sing-box-flutter/core/transport"
	wgc "github.com/idy/wgc"
	"golang.org/x/crypto/curve25519"
)

const Version = "0.2.0"
const SingBoxVersion = "1.14.4"
const MaxConfigBytes = 128 << 10

const maxPeers = 32
const maxNetworkRoutes = 1024
const maxExcludedRoutes = maxNetworkRoutes + maxPeers

type Peer struct {
	ID        string `json:"id"`
	PublicKey string `json:"public_key"`
	Endpoint  string `json:"endpoint"`
}

type NetworkSettings struct {
	Addresses      []string `json:"addresses,omitempty"`
	MTU            uint32   `json:"mtu,omitempty"`
	DNSServers     []string `json:"dns_servers"`
	IncludedRoutes []string `json:"included_routes"`
	ExcludedRoutes []string `json:"excluded_routes"`
}

type Config struct {
	Version     int             `json:"version"`
	PrivateKey  string          `json:"private_key"`
	Addresses   []string        `json:"addresses"`
	Peers       []Peer          `json:"peers"`
	DefaultPeer string          `json:"default_peer,omitempty"`
	Transport   transport.Spec  `json:"transport,omitempty"`
	MTU         uint32          `json:"mtu,omitempty"`
	DNSServer   string          `json:"dns_server,omitempty"`
	Network     NetworkSettings `json:"network,omitempty"`
	SingBox     json.RawMessage `json:"sing_box,omitempty"`
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func DecodeConfig(raw []byte) (Config, error) {
	var config Config
	if len(raw) == 0 || len(raw) > MaxConfigBytes {
		return config, errors.New("configuration size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	type plainConfig Config
	if decoder.Decode((*plainConfig)(&config)) != nil || decoder.Decode(new(any)) != io.EOF {
		return Config{}, errors.New("invalid configuration JSON")
	}
	return NormalizeConfig(config)
}

// UnmarshalJSON applies the same strict, redacted validation when Config is
// nested in a sing-box endpoint. It never opens a transport or resolves names.
func (config *Config) UnmarshalJSON(raw []byte) error {
	decoded, err := DecodeConfig(raw)
	if err != nil {
		return err
	}
	*config = decoded
	return nil
}

// Validate checks the legacy v1 configuration and built-in transport options
// offline. NetworkSettings are compatibility data; libbox owns OS VPN settings.
func Validate(config Config) (NetworkSettings, error) {
	config, err := NormalizeConfig(config)
	if err != nil {
		return NetworkSettings{}, err
	}
	if err := transport.Default.ValidateContext(context.Background(), config.Transport); err != nil {
		return NetworkSettings{}, errors.New("invalid WGC transport configuration")
	}
	return config.NetworkSettings(), nil
}

func parseKey(value string) (wgc.Key, error) {
	key, err := wgc.KeyFromBase64(value)
	if err != nil || key.IsZero() || base64.StdEncoding.EncodeToString(key[:]) != value {
		return wgc.Key{}, errors.New("invalid canonical X25519 key")
	}
	return key, nil
}

func numericAddress(value string) (netip.AddrPort, error) {
	address, err := netip.ParseAddrPort(value)
	if err != nil || address.Port() == 0 || address.Addr().Zone() != "" || address.Addr().IsUnspecified() || address.Addr().IsMulticast() {
		return netip.AddrPort{}, errors.New("numeric unicast address and nonzero port required")
	}
	return address, nil
}

func NormalizeConfig(config Config) (Config, error) {
	fail := func(message string) (Config, error) { return Config{}, errors.New(message) }
	if len(config.SingBox) != 0 {
		return fail("nested sing_box configuration is not supported by the WGC endpoint")
	}
	if config.Version != 1 {
		return fail("unsupported configuration version")
	}
	private, err := parseKey(config.PrivateKey)
	if err != nil {
		return fail("invalid private key")
	}
	if len(config.Addresses) == 0 || len(config.Addresses) > 8 {
		return fail("one to eight local addresses required")
	}
	var has4, has6 bool
	addresses := make(map[netip.Addr]bool, len(config.Addresses))
	for _, value := range config.Addresses {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Addr().IsUnspecified() || prefix.Addr().IsMulticast() || prefix.Addr().Is4In6() {
			return fail("invalid local address prefix")
		}
		if addresses[prefix.Addr()] {
			return fail("duplicate local address")
		}
		addresses[prefix.Addr()] = true
		if prefix.Addr().Is4() {
			has4 = true
		} else {
			has6 = true
		}
	}
	if len(config.Peers) == 0 || len(config.Peers) > maxPeers {
		return fail("one to 32 peers required")
	}
	ids := map[string]bool{}
	keys := map[wgc.Key]bool{}
	for _, peer := range config.Peers {
		if !validID.MatchString(peer.ID) || ids[peer.ID] {
			return fail("invalid or duplicate peer ID")
		}
		ids[peer.ID] = true
		key, err := parseKey(peer.PublicKey)
		if err != nil || keys[key] {
			return fail("invalid or duplicate peer public key")
		}
		keys[key] = true
		if _, err := curve25519.X25519(private[:], key[:]); err != nil {
			return fail("unusable peer public key")
		}
		if _, err := numericAddress(peer.Endpoint); err != nil {
			return fail("invalid peer endpoint")
		}
	}
	if config.DefaultPeer == "" {
		config.DefaultPeer = config.Peers[0].ID
	}
	if !ids[config.DefaultPeer] {
		return fail("default peer is not declared")
	}
	if config.MTU == 0 {
		config.MTU = 1280
	}
	if config.MTU < 1280 || config.MTU > 1420 {
		return fail("MTU must be between 1280 and 1420")
	}
	if config.DNSServer == "" {
		config.DNSServer = "1.1.1.1:53"
		if !has4 {
			config.DNSServer = "[2606:4700:4700::1111]:53"
		}
	}
	resolver, err := numericAddress(config.DNSServer)
	if err != nil || (resolver.Addr().Is4() && !has4) || (resolver.Addr().Is6() && !has6) {
		return fail("upstream DNS requires a configured address family")
	}
	if config.Transport.Type == "" {
		config.Transport.Type = "udp"
	}
	if len(config.Network.Addresses) > 0 || config.Network.MTU != 0 {
		return fail("network addresses and MTU are derived from top-level fields")
	}
	if config.Network.DNSServers == nil {
		if has4 {
			config.Network.DNSServers = []string{"198.18.0.2"}
		} else {
			config.Network.DNSServers = []string{"2001:2::2"}
		}
	}
	if config.Network.IncludedRoutes == nil {
		if has4 {
			config.Network.IncludedRoutes = append(config.Network.IncludedRoutes, "0.0.0.0/0")
		}
		if has6 {
			config.Network.IncludedRoutes = append(config.Network.IncludedRoutes, "::/0")
		}
	}
	if len(config.Network.IncludedRoutes) > maxNetworkRoutes || len(config.Network.ExcludedRoutes) > maxNetworkRoutes {
		return fail("too many routes")
	}
	// Validate the final ABI settings, including all derived peer exclusions.
	settings := config.NetworkSettings()
	if len(settings.ExcludedRoutes) > maxExcludedRoutes {
		return fail("too many derived excluded routes")
	}
	if len(settings.DNSServers) == 0 || len(settings.DNSServers) > 8 {
		return fail("one to eight tunnel DNS addresses required")
	}
	for _, value := range settings.DNSServers {
		ip, err := netip.ParseAddr(value)
		if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || ip.Is4In6() || (ip.Is4() && !has4) || (ip.Is6() && !has6) {
			return fail("invalid tunnel DNS address")
		}
	}
	for index, routes := range [][]string{settings.IncludedRoutes, settings.ExcludedRoutes} {
		for _, value := range routes {
			prefix, err := netip.ParsePrefix(value)
			if err != nil || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
				return fail("invalid network route")
			}
			if index == 0 && ((prefix.Addr().Is4() && !has4) || (prefix.Addr().Is6() && !has6)) {
				return fail("included route requires a configured address family")
			}
		}
	}
	return config, nil
}

func (config Config) NetworkSettings() NetworkSettings {
	settings := config.Network
	settings.Addresses = slices.Clone(config.Addresses)
	settings.MTU = config.MTU
	settings.DNSServers = slices.Clone(settings.DNSServers)
	settings.IncludedRoutes = slices.Clone(settings.IncludedRoutes)
	settings.ExcludedRoutes = slices.Clone(settings.ExcludedRoutes)
	seen := map[netip.Prefix]bool{}
	for _, route := range settings.ExcludedRoutes {
		prefix, err := netip.ParsePrefix(route)
		if err == nil {
			seen[prefix] = true
		}
	}
	for _, peer := range config.Peers {
		address, _ := netip.ParseAddrPort(peer.Endpoint)
		host := address.Addr().Unmap()
		route := netip.PrefixFrom(host, host.BitLen())
		if !seen[route] {
			settings.ExcludedRoutes = append(settings.ExcludedRoutes, route.String())
			seen[route] = true
		}
	}
	return settings
}
