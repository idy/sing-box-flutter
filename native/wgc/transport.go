// SPDX-License-Identifier: GPL-3.0-or-later
package wgc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"sync"

	wireguard "github.com/idy/wgc"
	udp "github.com/idy/wgc/transport"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

// TransportFactory is a compiled Go hook, not a per-packet Dart callback. Factories
// must honor context cancellation and unblock I/O on Close. NewUDPTransport can
// supply the protected base socket for custom wrappers.
type TransportFactory func(context.Context, netip.AddrPort, json.RawMessage) (wireguard.Transport, error)

var transportMu sync.RWMutex
var transports = map[string]TransportFactory{"udp": NewUDPTransport}

func RegisterTransport(name string, factory TransportFactory) error {
	transportMu.Lock()
	defer transportMu.Unlock()
	if name == "" || factory == nil || transports[name] != nil {
		return errors.New("invalid or duplicate WGC transport")
	}
	transports[name] = factory
	return nil
}

func transportFactory(name string) TransportFactory {
	if name == "" {
		name = "udp"
	}
	transportMu.RLock()
	defer transportMu.RUnlock()
	return transports[name]
}

func validUDPOptions(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var options map[string]json.RawMessage
	return json.Unmarshal(raw, &options) == nil && len(options) == 0
}

func NewUDPTransport(ctx context.Context, peer netip.AddrPort, options json.RawMessage) (wireguard.Transport, error) {
	if !validUDPOptions(options) {
		return nil, errors.New("the default WGC UDP transport has no options")
	}
	var conn net.PacketConn
	var err error
	if runtime.GOOS == "android" {
		manager := service.FromContext[adapter.NetworkManager](ctx)
		if manager == nil || manager.ProtectFunc() == nil {
			return nil, errors.New("WGC requires native Android socket protection")
		}
		listener := net.ListenConfig{Control: manager.ProtectFunc()}
		network := "udp4"
		if peer.Addr().Is6() {
			network = "udp6"
		}
		conn, err = listener.ListenPacket(ctx, network, ":0")
	} else {
		// Reuse sing-box's interface selection, monitoring, and socket policy.
		d, createErr := dialer.NewDefault(ctx, option.DialerOptions{})
		if createErr != nil {
			return nil, createErr
		}
		conn, err = d.ListenPacket(ctx, M.Socksaddr{Addr: peer.Addr(), Port: peer.Port()})
	}
	if err != nil {
		return nil, err
	}
	if socket, ok := conn.(*net.UDPConn); ok {
		return udp.FromUDPConn(socket), nil
	}
	return packetTransport{conn}, nil
}

type packetTransport struct{ net.PacketConn }

func (p packetTransport) ReadFrom(buffer []byte) (int, netip.AddrPort, error) {
	n, from, err := p.PacketConn.ReadFrom(buffer)
	if err != nil {
		return n, netip.AddrPort{}, err
	}
	address, err := netip.ParseAddrPort(from.String())
	if err != nil {
		return 0, netip.AddrPort{}, err
	}
	return n, netip.AddrPortFrom(address.Addr().Unmap(), address.Port()), nil
}

func (p packetTransport) WriteTo(buffer []byte, to netip.AddrPort) (int, error) {
	return p.PacketConn.WriteTo(buffer, net.UDPAddrFromAddrPort(to))
}
