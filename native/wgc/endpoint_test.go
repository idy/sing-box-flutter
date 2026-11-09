//go:build with_gvisor

package wgc

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wireguard "github.com/idy/wgc"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/wireguard-go/conn"
	"github.com/sagernet/wireguard-go/device"
	wgTun "github.com/sagernet/wireguard-go/tun"
)

func testKey(t *testing.T) (wireguard.Key, wireguard.Key) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var private, public wireguard.Key
	copy(private[:], key.Bytes())
	copy(public[:], key.PublicKey().Bytes())
	return private, public
}

func testOptions(t *testing.T) Options {
	private, _ := testKey(t)
	_, public := testKey(t)
	return Options{PrivateKey: private.String(), PeerPublicKey: public.String(), Address: []netip.Prefix{netip.MustParsePrefix("10.7.0.2/32")}, PeerEndpoint: netip.MustParseAddrPort("127.0.0.1:51820")}
}

func registerTestTransport(t *testing.T, name string, factory TransportFactory) {
	t.Helper()
	if err := RegisterTransport(name, factory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { transportMu.Lock(); delete(transports, name); transportMu.Unlock() })
}

func TestConstructionIsOfflineAndUsesExistingWGCOptions(t *testing.T) {
	var opened atomic.Int32
	registerTestTransport(t, "offline-test", func(context.Context, netip.AddrPort, json.RawMessage) (wireguard.Transport, error) {
		opened.Add(1)
		return nil, errors.New("must not open")
	})
	options := testOptions(t)
	options.Transport = "offline-test"
	options.PeerEndpoint = netip.MustParseAddrPort("[::ffff:203.0.113.10]:51820")
	created, err := NewEndpoint(context.Background(), nil, nil, "wgc-test", options)
	if err != nil {
		t.Fatal(err)
	}
	ep := created.(*Endpoint)
	defer ep.Close()
	if opened.Load() != 0 || ep.Type() != "wgc" || ep.Tag() != "wgc-test" || ep.options.PeerEndpoint.String() != "203.0.113.10:51820" {
		t.Fatal("construction performed I/O or failed address normalization")
	}
	options.Address[0] = netip.MustParsePrefix("10.9.0.2/32")
	if ep.options.Address[0].String() != "10.7.0.2/32" {
		t.Fatal("caller still owns endpoint address storage")
	}
	if _, err := ep.DialContext(context.Background(), "udp", M.ParseSocksaddr("10.7.0.1:53")); !errors.Is(err, net.ErrClosed) {
		t.Fatal("traffic accepted before startup")
	}
}

func TestInvalidOptionsFailWithoutLeakingKeys(t *testing.T) {
	for _, change := range []func(*Options){
		func(o *Options) { o.PrivateKey = "private-secret" },
		func(o *Options) { o.PeerPublicKey = "private-secret" },
		func(o *Options) { o.Address = nil },
		func(o *Options) { o.MTU = 1 },
		func(o *Options) { o.PeerEndpoint = netip.AddrPort{} },
		func(o *Options) { o.PeerEndpoint = netip.MustParseAddrPort("[::ffff:0.0.0.0]:51820") },
		func(o *Options) { o.Transport = "missing-private-secret" },
	} {
		o := testOptions(t)
		change(&o)
		if ep, err := NewEndpoint(context.Background(), nil, nil, "", o); ep != nil || err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("invalid options accepted or leaked")
		}
	}
}

func TestCanceledStartupAndClose(t *testing.T) {
	registerTestTransport(t, "cancel-test", func(ctx context.Context, _ netip.AddrPort, _ json.RawMessage) (wireguard.Transport, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	o := testOptions(t)
	o.Transport = "cancel-test"
	created, err := NewEndpoint(ctx, nil, nil, "", o)
	if err != nil {
		t.Fatal(err)
	}
	ep := created.(*Endpoint)
	scope := adapter.NewScope(ctx, nil)
	if err := ep.Start(adapter.StartStateInitialize, scope); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ep.Start(adapter.StartStateStart, scope) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled startup succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("startup ignored cancellation")
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	_ = ep.Close()
}

type countTransport struct {
	wireguard.Transport
	writes *atomic.Int32
}

func (t countTransport) WriteTo(data []byte, address netip.AddrPort) (int, error) {
	t.writes.Add(1)
	return t.Transport.WriteTo(data, address)
}

type memoryTun struct {
	outbound, received chan []byte
	events             chan wgTun.Event
	done               chan struct{}
	once               sync.Once
}

func (t *memoryTun) File() *os.File             { return nil }
func (t *memoryTun) Name() (string, error)      { return "wgc-interop", nil }
func (t *memoryTun) MTU() (int, error)          { return 1280, nil }
func (t *memoryTun) BatchSize() int             { return 1 }
func (t *memoryTun) Events() <-chan wgTun.Event { return t.events }
func (t *memoryTun) Close() error               { t.once.Do(func() { close(t.done); close(t.events) }); return nil }
func (t *memoryTun) Read(buffers [][]byte, sizes []int, offset int) (int, error) {
	select {
	case <-t.done:
		return 0, net.ErrClosed
	case packet := <-t.outbound:
		sizes[0] = copy(buffers[0][offset:], packet)
		return 1, nil
	}
}
func (t *memoryTun) Write(buffers [][]byte, offset int) (int, error) {
	for i, packet := range buffers {
		select {
		case <-t.done:
			return i, net.ErrClosed
		case t.received <- append([]byte(nil), packet[offset:]...):
		}
	}
	return len(buffers), nil
}

// The remote uses stock wireguard-go and an in-memory TUN; host routes never change.
func TestStockWireGuardRoundTripThroughRegisteredTransport(t *testing.T) {
	clientPrivate, clientPublic := testKey(t)
	serverPrivate, serverPublic := testKey(t)
	tun := &memoryTun{outbound: make(chan []byte, 8), received: make(chan []byte, 8), events: make(chan wgTun.Event, 1), done: make(chan struct{})}
	server := device.NewDevice(context.Background(), tun, conn.NewDefaultBind(nil), device.NewLogger(device.LogLevelSilent, ""), 1)
	defer server.Close()
	if err := server.IpcSet(fmt.Sprintf("private_key=%x\nlisten_port=0\npublic_key=%x\nallowed_ip=10.7.0.2/32\n", serverPrivate[:], clientPublic[:])); err != nil {
		t.Fatal(err)
	}
	if err := server.Up(); err != nil {
		t.Fatal(err)
	}
	ipc, err := server.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	var port string
	for _, line := range strings.Split(ipc, "\n") {
		if strings.HasPrefix(line, "listen_port=") {
			port = strings.TrimPrefix(line, "listen_port=")
		}
	}
	if port == "" || port == "0" {
		t.Fatal("peer socket not bound")
	}
	var writes atomic.Int32
	registerTestTransport(t, "count-test", func(ctx context.Context, peer netip.AddrPort, options json.RawMessage) (wireguard.Transport, error) {
		base, err := NewUDPTransport(ctx, peer, options)
		if err != nil {
			return nil, err
		}
		return countTransport{base, &writes}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	o := Options{Address: []netip.Prefix{netip.MustParsePrefix("10.7.0.2/32")}, PrivateKey: clientPrivate.String(), PeerPublicKey: serverPublic.String(), PeerEndpoint: netip.MustParseAddrPort(net.JoinHostPort("127.0.0.1", port)), Transport: "count-test"}
	created, err := NewEndpoint(ctx, nil, nil, "wgc", o)
	if err != nil {
		t.Fatal(err)
	}
	ep := created.(*Endpoint)
	scope := adapter.NewScope(ctx, nil)
	defer scope.Close()
	for _, stage := range []adapter.StartStage{adapter.StartStateInitialize, adapter.StartStateStart} {
		if err := ep.Start(stage, scope); err != nil {
			t.Fatal(err)
		}
	}
	udp, err := ep.DialContext(ctx, "udp", M.ParseSocksaddr("10.7.0.1:8081"))
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	originalConnection, originalDevice := ep.conn, ep.device
	for round := 0; round < 2; round++ {
		if round == 1 {
			previous, changed := ep.binding.snapshot()
			// Model the old interface disappearing before its update notification.
			_ = previous.Transport.Close()
			ep.InterfaceUpdated(ctx)
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatal("handover did not reopen the transport")
			}
		}
		payload := fmt.Sprintf("sing-box-wgc-%d", round)
		var packet []byte
		for attempt := 0; attempt < 8 && packet == nil; attempt++ {
			if _, err := udp.Write([]byte(payload)); err != nil {
				t.Fatal(err)
			}
			select {
			case candidate := <-tun.received:
				if len(candidate) >= 28 {
					headerLength := int(candidate[0]&15) * 4
					if headerLength+8 <= len(candidate) && bytes.Equal(candidate[headerLength+8:], []byte(payload)) {
						packet = candidate
					}
				}
			case <-time.After(300 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if len(packet) < 28 || packet[0]>>4 != 4 || packet[9] != 17 {
			t.Fatal("stock peer did not receive IPv4 UDP")
		}
		h := int(packet[0]&15) * 4
		if !bytes.Equal(packet[h+8:], []byte(payload)) {
			t.Fatal("WGC payload changed")
		}
		reply := append([]byte(nil), packet...)
		copy(reply[12:16], packet[16:20])
		copy(reply[16:20], packet[12:16])
		copy(reply[h:h+2], packet[h+2:h+4])
		copy(reply[h+2:h+4], packet[h:h+2])
		reply[h+6], reply[h+7], reply[10], reply[11] = 0, 0, 0, 0
		var sum uint32
		for i := 0; i < h; i += 2 {
			sum += uint32(binary.BigEndian.Uint16(reply[i : i+2]))
		}
		for sum>>16 != 0 {
			sum = (sum & 0xffff) + (sum >> 16)
		}
		binary.BigEndian.PutUint16(reply[10:12], ^uint16(sum))
		tun.outbound <- reply
		_ = udp.SetReadDeadline(time.Now().Add(3 * time.Second))
		output := make([]byte, 64)
		n, err := udp.Read(output)
		if err != nil || string(output[:n]) != payload || writes.Load() == 0 {
			t.Fatalf("WGC round trip failed: %v", err)
		}
	}
	if ep.conn != originalConnection || ep.device != originalDevice {
		t.Fatal("handover restarted the WGC session or userspace stack")
	}
	_ = ep.Close()
	if _, err := ep.ListenPacket(ctx, M.ParseSocksaddr("10.7.0.1:53")); !errors.Is(err, net.ErrClosed) {
		t.Fatal("endpoint accepted traffic after close")
	}
}
