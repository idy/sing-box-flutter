package core

import (
	"context"
	"errors"
	"net"
	"net/netip"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/dialer"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

// peerOutbound preserves the legacy DNS candidate ordering and peer admission
// path. Endpoint supplies a live session for each operation.
type peerOutbound struct {
	ctx            context.Context
	session        *Session
	peer, resolver string
}

var _ dialer.PacketDialerWithDestination = (*peerOutbound)(nil)

// numericPeerDialer keeps every candidate behind the session's peer policy.
// It must not implement N.ParallelDialer: DialParallel uses DialSerial internally,
// which would dispatch back to DialParallel and recurse.
type numericPeerDialer struct {
	session *Session
	peer    string
}

var _ N.Dialer = numericPeerDialer{}

func (d numericPeerDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := d.session.DialPeer(ctx, d.peer, network, destination.String())
	if err == nil && ctx.Err() != nil {
		_ = conn.Close()
		return nil, ctx.Err()
	}
	return conn, err
}

func (d numericPeerDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := d.session.listenPeer(ctx, d.peer, destination)
	if err == nil && ctx.Err() != nil {
		_ = conn.Close()
		return nil, ctx.Err()
	}
	return conn, err
}

func (o *peerOutbound) addresses(ctx context.Context, destination M.Socksaddr) ([]netip.Addr, error) {
	if !destination.IsValid() || destination.Port == 0 {
		return nil, errors.New("invalid destination")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !destination.IsDomain() {
		return []netip.Addr{destination.Addr}, nil
	}
	manager := service.FromContext[adapter.DNSTransportManager](o.ctx)
	router := service.FromContext[adapter.DNSRouter](o.ctx)
	if manager == nil || router == nil {
		return nil, errors.New("WGC outbound DNS services unavailable")
	}
	resolver, found := manager.Transport(o.resolver)
	if !found || resolver == nil {
		return nil, errors.New("WGC outbound DNS transport unavailable")
	}
	addresses, err := router.Lookup(ctx, destination.Fqdn, adapter.DNSQueryOptions{Transport: resolver})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("WGC outbound DNS lookup failed")
	}
	if len(addresses) == 0 {
		return nil, errors.New("WGC outbound DNS returned no addresses")
	}
	return addresses, nil
}
func (o *peerOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	addresses, err := o.addresses(ctx, destination)
	if err != nil {
		return nil, err
	}
	peerDialer := numericPeerDialer{session: o.session, peer: o.peer}
	var conn net.Conn
	if destination.IsDomain() && N.NetworkName(network) == N.NetworkTCP {
		// DNS order determines the preferred family. Sing races the other family
		// after its default 300 ms delay; addresses within each family stay serial.
		conn, err = N.DialParallel(ctx, peerDialer, network, destination, addresses, addresses[0].Is6() && !addresses[0].Is4In6(), 0)
	} else {
		conn, err = N.DialSerial(ctx, peerDialer, network, destination, addresses)
	}
	if err == nil {
		return conn, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, errors.New("WGC destination unavailable")
}
func (o *peerOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	conn, _, err := o.ListenPacketWithDestination(ctx, destination)
	return conn, err
}
func (o *peerOutbound) ListenPacketWithDestination(ctx context.Context, destination M.Socksaddr) (net.PacketConn, netip.Addr, error) {
	addresses, err := o.addresses(ctx, destination)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	conn, address, err := N.ListenSerial(ctx, numericPeerDialer{session: o.session, peer: o.peer}, destination, addresses)
	if err != nil {
		if ctx.Err() != nil {
			return nil, netip.Addr{}, ctx.Err()
		}
		return nil, netip.Addr{}, errors.New("WGC destination unavailable")
	}
	return conn, address, nil
}
