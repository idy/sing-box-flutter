//go:build with_gvisor

package core

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	sg "github.com/sagernet/sing-box/transport/wireguard"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

// A domain is also what reaches the outbound after sing-box reverses a FakeIP.
var outboundTestDomain = M.Socksaddr{Fqdn: "outbound.example.test", Port: 8080}

type outboundDNSRouter struct {
	adapter.DNSRouter
	t         *testing.T
	transport adapter.DNSTransport
	addresses []netip.Addr
	calls     atomic.Int32
}

func (r *outboundDNSRouter) Lookup(_ context.Context, domain string, options adapter.DNSQueryOptions) ([]netip.Addr, error) {
	r.calls.Add(1)
	if domain != outboundTestDomain.Fqdn || options.Transport != r.transport {
		r.t.Errorf("lookup bypassed configured resolver: domain=%q transport=%v", domain, options.Transport)
	}
	return append([]netip.Addr(nil), r.addresses...), nil
}

type outboundDNSManager struct {
	adapter.DNSTransportManager
	transport adapter.DNSTransport
}

func (m *outboundDNSManager) Transport(tag string) (adapter.DNSTransport, bool) {
	return m.transport, tag == "outbound-test-resolver"
}

type outboundDNSTransport struct{ adapter.DNSTransport }

type outboundPeerDevice struct {
	sg.Device
	dial   func(context.Context, string, M.Socksaddr) (net.Conn, error)
	listen func(context.Context, M.Socksaddr) (net.PacketConn, error)
}

func (d *outboundPeerDevice) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return d.dial(ctx, network, destination)
}

func (d *outboundPeerDevice) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return d.listen(ctx, destination)
}

func newTestPeerOutbound(t *testing.T, device sg.Device, addresses ...netip.Addr) (*peerOutbound, *outboundDNSRouter) {
	t.Helper()
	resolver := &outboundDNSTransport{}
	router := &outboundDNSRouter{t: t, transport: resolver, addresses: addresses}
	ctx := service.ContextWith[adapter.DNSRouter](context.Background(), router)
	ctx = service.ContextWith[adapter.DNSTransportManager](ctx, &outboundDNSManager{transport: resolver})
	session := &Session{ctx: context.Background(), peers: map[string]*peerDevice{"test": {device: device}}}
	return &peerOutbound{ctx: ctx, session: session, peer: "test", resolver: "outbound-test-resolver"}, router
}

func TestPeerOutboundUDPDomainFamilyFallback(t *testing.T) {
	for _, test := range []struct {
		name, local, first, usable string
	}{
		{"IPv6_only_A_first", "fd07::2/128", "10.7.0.1", "fd07::1"},
		{"IPv4_only_AAAA_first", "10.7.0.2/32", "fd07::1", "10.7.0.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			device, err := sg.NewDevice(sg.DeviceOptions{
				Context: context.Background(), MTU: 1280,
				Address: []netip.Prefix{netip.MustParsePrefix(test.local)},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = device.Close() })
			if err := device.Start(); err != nil {
				t.Fatal(err)
			}
			var attempted []M.Socksaddr
			observed := &outboundPeerDevice{Device: device, listen: func(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
				attempted = append(attempted, destination)
				return device.ListenPacket(ctx, destination)
			}}
			first, usable := netip.MustParseAddr(test.first), netip.MustParseAddr(test.usable)
			outbound, router := newTestPeerOutbound(t, observed, first, usable)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, selected, err := outbound.ListenPacketWithDestination(ctx, outboundTestDomain)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if selected != usable {
				t.Fatalf("selected %v, want usable address %v", selected, usable)
			}
			if len(attempted) != 2 || attempted[0] != M.SocksaddrFrom(first, outboundTestDomain.Port) || attempted[1] != M.SocksaddrFrom(usable, outboundTestDomain.Port) {
				t.Fatalf("candidate order or port changed: %v", attempted)
			}
			local, err := netip.ParseAddrPort(conn.LocalAddr().String())
			if err != nil || local.Addr() != netip.MustParsePrefix(test.local).Addr() {
				t.Fatalf("packet socket uses wrong local family: %v (%v)", conn.LocalAddr(), err)
			}
			if _, err := conn.WriteTo([]byte("udp-fallback"), &net.UDPAddr{IP: net.IP(selected.AsSlice()), Port: int(outboundTestDomain.Port)}); err != nil {
				t.Fatalf("selected address cannot use packet socket: %v", err)
			}
			if router.calls.Load() != 1 {
				t.Fatalf("DNS lookups = %d, want 1", router.calls.Load())
			}
		})
	}
}

func TestPeerOutboundCancellationStopsCandidates(t *testing.T) {
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var attempts atomic.Int32
			fail := func(destination M.Socksaddr) error {
				if attempts.Add(1) != 1 || destination.Addr != netip.MustParseAddr("10.7.0.1") {
					t.Errorf("attempted candidate after cancellation: %v", destination)
				}
				cancel()
				return errors.New("unavailable address family")
			}
			device := &outboundPeerDevice{
				dial: func(_ context.Context, _ string, destination M.Socksaddr) (net.Conn, error) {
					return nil, fail(destination)
				},
				listen: func(_ context.Context, destination M.Socksaddr) (net.PacketConn, error) {
					return nil, fail(destination)
				},
			}
			outbound, _ := newTestPeerOutbound(t, device, netip.MustParseAddr("10.7.0.1"), netip.MustParseAddr("fd07::1"))
			if network == "udp" {
				conn, selected, err := outbound.ListenPacketWithDestination(ctx, outboundTestDomain)
				if conn != nil || selected.IsValid() || !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled packet listen: conn=%v selected=%v err=%v", conn, selected, err)
				}
			} else {
				conn, err := outbound.DialContext(ctx, network, outboundTestDomain)
				if conn != nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled dial: conn=%v err=%v", conn, err)
				}
			}
			if attempts.Load() != 1 {
				t.Fatalf("device attempts = %d, want 1", attempts.Load())
			}
		})
	}
}

type outboundTrackedConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *outboundTrackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

func newOutboundTrackedConn(t *testing.T) *outboundTrackedConn {
	t.Helper()
	client, server := net.Pipe()
	conn := &outboundTrackedConn{Conn: client, closed: make(chan struct{})}
	t.Cleanup(func() { _ = conn.Close(); _ = server.Close() })
	return conn
}

func waitOutboundSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func TestPeerOutboundTCPDomainHappyEyeballs(t *testing.T) {
	for _, preferIPv6 := range []bool{false, true} {
		name := "A_first"
		if preferIPv6 {
			name = "AAAA_first"
		}
		t.Run(name, func(t *testing.T) {
			first, second := netip.MustParseAddr("10.7.0.1"), netip.MustParseAddr("fd07::1")
			if preferIPv6 {
				first, second = second, first
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			winner := newOutboundTrackedConn(t)
			primaryStarted, primaryCanceled := make(chan struct{}), make(chan struct{})
			device := &outboundPeerDevice{dial: func(attemptCtx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
				if got, ok := attemptCtx.Deadline(); !ok || got != deadline {
					t.Errorf("caller deadline changed: %v, %v", got, ok)
				}
				if network != "tcp" || destination.IsDomain() || destination.Port != outboundTestDomain.Port {
					t.Errorf("non-numeric peer attempt or changed network/port: %s %v", network, destination)
				}
				switch destination.Addr {
				case first:
					close(primaryStarted)
					<-attemptCtx.Done() // Simulate a blackhole, not an immediate family error.
					close(primaryCanceled)
					return nil, attemptCtx.Err()
				case second:
					select {
					case <-primaryStarted:
					default:
						t.Error("DNS family preference was ignored")
					}
					return winner, nil
				default:
					return nil, errors.New("unexpected candidate")
				}
			}}
			outbound, router := newTestPeerOutbound(t, device, first, second)
			started := time.Now()
			conn, err := outbound.DialContext(ctx, "tcp", outboundTestDomain)
			elapsed := time.Since(started)
			if err != nil || conn != winner {
				t.Fatalf("reachable fallback starved by blackhole: conn=%v err=%v", conn, err)
			}
			if elapsed < N.DefaultFallbackDelay/2 || elapsed > 2*time.Second {
				t.Fatalf("fallback completed after %v, want default %v delay before caller deadline", elapsed, N.DefaultFallbackDelay)
			}
			waitOutboundSignal(t, primaryCanceled, "losing family was not canceled")
			if ctx.Err() != nil {
				t.Fatalf("caller context canceled by race: %v", ctx.Err())
			}
			select {
			case <-winner.closed:
				t.Fatal("winning connection was closed")
			default:
			}
			if router.calls.Load() != 1 {
				t.Fatalf("DNS lookups = %d, want 1", router.calls.Load())
			}
		})
	}
}

func TestPeerOutboundTCPDomainSameFamilySerial(t *testing.T) {
	for _, addresses := range [][]netip.Addr{
		{netip.MustParseAddr("10.7.0.1"), netip.MustParseAddr("10.7.0.3")},
		{netip.MustParseAddr("fd07::1"), netip.MustParseAddr("fd07::3")},
	} {
		t.Run(addresses[0].String(), func(t *testing.T) {
			winner := newOutboundTrackedConn(t)
			var attempted []netip.Addr
			device := &outboundPeerDevice{dial: func(_ context.Context, _ string, destination M.Socksaddr) (net.Conn, error) {
				attempted = append(attempted, destination.Addr)
				if destination.Addr == addresses[0] {
					return nil, errors.New("first address unavailable")
				}
				return winner, nil
			}}
			outbound, _ := newTestPeerOutbound(t, device, addresses...)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, err := outbound.DialContext(ctx, "tcp", outboundTestDomain)
			if err != nil || conn != winner {
				t.Fatalf("same-family fallback: conn=%v err=%v", conn, err)
			}
			if len(attempted) != 2 || attempted[0] != addresses[0] || attempted[1] != addresses[1] {
				t.Fatalf("same-family candidate order changed: %v", attempted)
			}
		})
	}
}

func TestPeerOutboundTCPClosesLateLoser(t *testing.T) {
	for _, primaryWins := range []bool{false, true} {
		name := "fallback_wins"
		if primaryWins {
			name = "primary_wins"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			primary, fallback := newOutboundTrackedConn(t), newOutboundTrackedConn(t)
			winner, loser := fallback, primary
			if primaryWins {
				winner, loser = primary, fallback
			}
			fallbackStarted, loserCanceled := make(chan struct{}), make(chan struct{})
			device := &outboundPeerDevice{dial: func(attemptCtx context.Context, _ string, destination M.Socksaddr) (net.Conn, error) {
				isPrimary := destination.Addr.Is4()
				if !isPrimary {
					close(fallbackStarted)
				}
				if isPrimary == primaryWins {
					select {
					case <-fallbackStarted:
						return winner, nil
					case <-attemptCtx.Done():
						return nil, attemptCtx.Err()
					}
				}
				<-attemptCtx.Done()
				close(loserCanceled)
				// A device may finish connecting concurrently with cancellation.
				return loser, nil
			}}
			outbound, _ := newTestPeerOutbound(t, device, netip.MustParseAddr("10.7.0.1"), netip.MustParseAddr("fd07::1"))
			conn, err := outbound.DialContext(ctx, "tcp", outboundTestDomain)
			if err != nil || conn != winner {
				t.Fatalf("wrong winning connection: conn=%v err=%v", conn, err)
			}
			waitOutboundSignal(t, loserCanceled, "losing attempt was not canceled")
			waitOutboundSignal(t, loser.closed, "late successful loser was not closed")
			select {
			case <-winner.closed:
				t.Fatal("winning connection was closed with loser")
			default:
			}
		})
	}
}

func TestPeerOutboundNumericDestinationsSkipDNS(t *testing.T) {
	for _, ip := range []string{"10.7.0.1", "fd07::1"} {
		t.Run(ip, func(t *testing.T) {
			destination := M.SocksaddrFrom(netip.MustParseAddr(ip), 8080)
			var dialed, listened int
			device := &outboundPeerDevice{
				dial: func(_ context.Context, _ string, got M.Socksaddr) (net.Conn, error) {
					dialed++
					if got != destination {
						t.Errorf("numeric destination changed: %v", got)
					}
					return newOutboundTrackedConn(t), nil
				},
				listen: func(_ context.Context, got M.Socksaddr) (net.PacketConn, error) {
					listened++
					if got != destination {
						t.Errorf("numeric packet destination changed: %v", got)
					}
					return &outboundNumericPacketConn{}, nil
				},
			}
			outbound, router := newTestPeerOutbound(t, device)
			// A numeric destination works even with no DNS services registered.
			outbound.ctx = context.Background()
			for _, network := range []string{"tcp", "udp"} {
				conn, err := outbound.DialContext(context.Background(), network, destination)
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.Close()
			}
			conn, selected, err := outbound.ListenPacketWithDestination(context.Background(), destination)
			if err != nil || selected != destination.Addr {
				t.Fatalf("numeric packet listen: selected=%v err=%v", selected, err)
			}
			_ = conn.Close()
			if dialed != 2 || listened != 1 || router.calls.Load() != 0 {
				t.Fatalf("numeric attempts: dials=%d listens=%d lookups=%d", dialed, listened, router.calls.Load())
			}
		})
	}
}

func TestPeerOutboundPreservesSessionPolicy(t *testing.T) {
	for _, policy := range []string{"closed_session", "undeclared_peer"} {
		t.Run(policy, func(t *testing.T) {
			device := &outboundPeerDevice{
				dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
					t.Error("dial bypassed session policy")
					return nil, errors.New("policy bypassed")
				},
				listen: func(context.Context, M.Socksaddr) (net.PacketConn, error) {
					t.Error("packet listen bypassed session policy")
					return nil, errors.New("policy bypassed")
				},
			}
			outbound, _ := newTestPeerOutbound(t, device, netip.MustParseAddr("10.7.0.1"))
			if policy == "closed_session" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				outbound.session.ctx = ctx
			} else {
				outbound.peer = "undeclared"
			}
			if conn, err := outbound.DialContext(context.Background(), "tcp", outboundTestDomain); err == nil || conn != nil {
				t.Fatalf("session policy accepted dial: conn=%v err=%v", conn, err)
			}
			if conn, selected, err := outbound.ListenPacketWithDestination(context.Background(), outboundTestDomain); err == nil || conn != nil || selected.IsValid() {
				t.Fatalf("session policy accepted packet listen: conn=%v selected=%v err=%v", conn, selected, err)
			}
		})
	}
}

type outboundNumericPacketConn struct{ net.PacketConn }

func (*outboundNumericPacketConn) Close() error { return nil }
