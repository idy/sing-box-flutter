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
	wgstack "github.com/sagernet/sing-box/transport/wireguard"
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

// A faulting reader retains the real sing-box device for startup, dialing and
// cleanup. The oversized packet forces a real WGC WriteTo admission failure.
type faultDevice struct {
	wgstack.Device
	fault      <-chan struct{}
	writeFault bool
	reads      int
	done       chan struct{}
	draining   chan struct{}
	blocked    chan struct{}
	released   chan struct{}
	once       sync.Once
}

func (d *faultDevice) Read(buffers [][]byte, sizes []int, offset int) (int, error) {
	d.reads++
	if d.reads == 1 {
		select {
		case <-d.done:
			return 0, net.ErrClosed
		case <-d.fault:
		}
		if !d.writeFault {
			return 0, errors.New("injected stack read failure")
		}
		sizes[0] = len(buffers[0]) - offset
		return 1, nil
	}
	close(d.draining)
	<-d.done
	return 0, net.ErrClosed
}

func (d *faultDevice) Write([][]byte, int) (int, error) {
	close(d.blocked)
	if d.writeFault {
		// A full outbound queue needs the sender to drain the response.
		select {
		case <-d.draining:
		case <-d.done:
		}
	} else {
		// A broken reader must close the stack to release this injection.
		<-d.done
	}
	close(d.released)
	return 0, net.ErrClosed
}

func (d *faultDevice) Close() error {
	d.once.Do(func() { close(d.done); _ = d.Device.Close() })
	return nil
}

func startTestEndpoint(t *testing.T, ep *Endpoint, scope *adapter.Scope) {
	t.Helper()
	t.Cleanup(func() { closeTestEndpoint(t, ep); _ = scope.Close() })
	for _, stage := range []adapter.StartStage{adapter.StartStateInitialize, adapter.StartStateStart} {
		if err := ep.Start(stage, scope); err != nil {
			t.Fatal(err)
		}
	}
}

func closeTestEndpoint(t *testing.T, ep *Endpoint) {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = ep.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("endpoint cleanup did not join its workers")
	}
}

func TestPacketLoopFailureRecoversEndpoint(t *testing.T) {
	for _, writeFault := range []bool{false, true} {
		name := "read"
		if writeFault {
			name = "write"
		}
		t.Run(name, func(t *testing.T) {
			server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			o := testOptions(t)
			o.PeerEndpoint = server.LocalAddr().(*net.UDPAddr).AddrPort()
			created, err := NewEndpoint(context.Background(), nil, nil, "", o)
			if err != nil {
				t.Fatal(err)
			}
			ep := created.(*Endpoint)
			if writeFault {
				// Bypass validation only to inject a fatal upstream queue error.
				ep.options.MTU = wireguard.MaximumDataSize + 1
			}
			fault := make(chan struct{})
			var initial *faultDevice
			var deviceCalls atomic.Int32
			ep.newDevice = func(options wgstack.DeviceOptions) (wgstack.Device, error) {
				base, err := wgstack.NewDevice(options)
				if err == nil && deviceCalls.Add(1) == 1 {
					initial = &faultDevice{Device: base, fault: fault, writeFault: writeFault, done: make(chan struct{}), draining: make(chan struct{}), blocked: make(chan struct{}), released: make(chan struct{})}
					return initial, nil
				}
				return base, err
			}
			scope := adapter.NewScope(context.Background(), nil)
			startTestEndpoint(t, ep, scope)
			oldCtx, oldConn, oldBinding := ep.runCtx, ep.conn, ep.binding
			// Model the receiver already injecting a packet whose stack response
			// cannot progress until the sender drains or closes the broken stack.
			ep.receivers.Add(1)
			go func() {
				defer ep.receivers.Done()
				_, _ = initial.Write([][]byte{{1}}, 0)
			}()
			<-initial.blocked
			close(fault)
			select {
			case <-oldCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("fatal packet error did not stop its session")
			}
			if _, err := ep.current(); !errors.Is(err, net.ErrClosed) {
				t.Fatal("failed session continued accepting traffic")
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				ep.mu.Lock()
				recovered := !ep.closed && ep.runCtx != oldCtx && ep.runCtx.Err() == nil && ep.dialer != nil
				ep.mu.Unlock()
				if recovered {
					break
				}
				select {
				case <-tick.C:
				case <-deadline.C:
					t.Fatal("fatal packet error permanently disabled the endpoint")
				}
			}
			select {
			case <-initial.done:
			default:
				t.Fatal("recovery leaked the failed userspace stack")
			}
			select {
			case <-initial.released:
			default:
				t.Fatal("recovery did not release the blocked receiver")
			}
			if active, _ := oldBinding.snapshot(); active != nil {
				t.Fatal("recovery leaked the failed transport")
			}
			if writeFault {
				select {
				case <-initial.draining:
				default:
					t.Fatal("fatal write stopped draining before stack cleanup")
				}
			}
			if _, _, err := oldConn.WriteTo(nil, ep.peerKey); err == nil {
				t.Fatal("recovery left the old WGC connection open")
			}
			udp, err := ep.DialContext(context.Background(), "udp", M.ParseSocksaddr("10.7.0.1:53"))
			if err != nil {
				t.Fatal(err)
			}
			defer udp.Close()
			if _, err := udp.Write([]byte("after-recovery")); err != nil {
				t.Fatal(err)
			}
			_ = server.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, _, err := server.ReadFromUDP(make([]byte, 2048)); err != nil {
				t.Fatalf("recovered packet sender did not reach its transport: %v", err)
			}
			closeTestEndpoint(t, ep)
			if active, _ := ep.binding.snapshot(); active != nil {
				t.Fatal("Close left the recovered transport open")
			}
			if deviceCalls.Load() != 2 {
				t.Fatal("recovery did not create exactly one replacement stack")
			}
		})
	}
}

func TestCanceledReadFailureReleasesBlockedReceiver(t *testing.T) {
	for _, stop := range []string{"close", "context", "scope"} {
		t.Run(stop, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			created, err := NewEndpoint(ctx, nil, nil, "", testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			ep := created.(*Endpoint)
			fault := make(chan struct{})
			var initial *faultDevice
			var calls atomic.Int32
			ep.newDevice = func(options wgstack.DeviceOptions) (wgstack.Device, error) {
				base, err := wgstack.NewDevice(options)
				if err == nil && calls.Add(1) == 1 {
					initial = &faultDevice{Device: base, fault: fault, done: make(chan struct{}), blocked: make(chan struct{}), released: make(chan struct{})}
					return initial, nil
				}
				return base, err
			}
			scope := adapter.NewScope(context.Background(), nil)
			startTestEndpoint(t, ep, scope)
			// Release the injection even on failure, before endpoint cleanup joins it.
			t.Cleanup(func() { _ = initial.Close() })
			runCtx := ep.runCtx
			// Model a receiver that entered Write while the session was still live.
			ep.receivers.Add(1)
			go func() {
				defer ep.receivers.Done()
				_, _ = initial.Write([][]byte{{1}}, 0)
			}()
			select {
			case <-initial.blocked:
			case <-time.After(time.Second):
				t.Fatal("receiver did not enter its stack write")
			}
			closed := make(chan error, 1)
			go func() {
				switch stop {
				case "close":
					closed <- ep.Close()
				case "context":
					cancel()
					closed <- ep.Close()
				case "scope":
					closed <- scope.Close()
				}
			}()
			select {
			case <-runCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("shutdown did not cancel the session")
			}
			// Cancellation must precede the Read error, with Write still blocked.
			select {
			case <-initial.released:
				t.Fatal("receiver was released before the read failure")
			default:
			}
			close(fault)
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown left a receiver blocked after a canceled-context read error")
			}
			select {
			case <-initial.released:
			default:
				t.Fatal("shutdown did not release the blocked receiver")
			}
			if calls.Load() != 1 {
				t.Fatal("intentional shutdown restarted the endpoint")
			}
			if active, _ := ep.binding.snapshot(); active != nil {
				t.Fatal("shutdown leaked its transport")
			}
		})
	}
}

func TestShutdownDoesNotRecoverEndpoint(t *testing.T) {
	for _, stop := range []string{"close", "context", "scope"} {
		t.Run(stop, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			created, err := NewEndpoint(ctx, nil, nil, "", testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			ep := created.(*Endpoint)
			var calls atomic.Int32
			ep.newDevice = func(options wgstack.DeviceOptions) (wgstack.Device, error) {
				calls.Add(1)
				return wgstack.NewDevice(options)
			}
			scope := adapter.NewScope(context.Background(), nil)
			startTestEndpoint(t, ep, scope)
			switch stop {
			case "context":
				cancel()
			case "scope":
				if err := scope.Close(); err != nil {
					t.Fatal(err)
				}
			}
			closeTestEndpoint(t, ep)
			if calls.Load() != 1 {
				t.Fatal("intentional shutdown restarted the endpoint")
			}
			if active, _ := ep.binding.snapshot(); active != nil {
				t.Fatal("shutdown leaked its transport")
			}
		})
	}
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
