//go:build with_gvisor

package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreTransport "github.com/idy/sing-box-flutter/core/transport"
	wgc "github.com/idy/wgc"
	udp "github.com/idy/wgc/transport"
	sg "github.com/sagernet/sing-box/transport/wireguard"
	M "github.com/sagernet/sing/common/metadata"
)

type sessionTestPeer struct {
	conn     *wgc.Conn
	public   string
	endpoint string
	packets  chan []byte
}

func newSessionTestPeer(t *testing.T, clientPublic string) *sessionTestPeer {
	t.Helper()
	private, public := testKeys(t)
	privateKey, _ := parseKey(private)
	clientKey, _ := parseKey(clientPublic)
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := wgc.NewConn(udp.FromUDPConn(socket), wgc.Config{
		StaticPrivateKey: &privateKey, DisableStats: true, SharedWriteQueue: true,
		MaxPeers: 1, ReadQueueSize: 128, WriteQueueSize: 128,
		PeerConfigLookup: func(key wgc.Key) (*wgc.PeerConfig, error) {
			if key != clientKey {
				return nil, nil
			}
			return &wgc.PeerConfig{}, nil
		},
	})
	if err != nil {
		_ = socket.Close()
		t.Fatal(err)
	}
	peer := &sessionTestPeer{conn: conn, public: public, endpoint: socket.LocalAddr().String(), packets: make(chan []byte, 16)}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for packet := range conn.ReadQueue() {
			payload := append([]byte(nil), packet.Data...)
			packet.Release()
			if len(payload) > 0 {
				select {
				case peer.packets <- payload:
				case <-stop:
				}
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = conn.Close()
		<-done
	})
	return peer
}

func sessionTestUDP(t *testing.T, session *Session, id string) net.Conn {
	t.Helper()
	conn, err := session.DialPeer(context.Background(), id, "udp", "10.7.0.1:8081")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// WGC drops data that initiates a handshake. Retry until the encrypted peer
// receives a packet so subsequent tests exercise an established session.
func warmSessionTestPeer(t *testing.T, conn net.Conn, peer *sessionTestPeer) []byte {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	retry := time.NewTicker(20 * time.Millisecond)
	defer retry.Stop()
	for {
		if _, err := conn.Write([]byte("warmup")); err != nil {
			t.Fatal(err)
		}
		select {
		case packet := <-peer.packets:
			return packet
		case <-retry.C:
		case <-deadline.C:
			t.Fatal("encrypted peer did not receive a warmup packet")
		}
	}
}

func sessionTestUDPReply(t *testing.T, packet []byte) []byte {
	t.Helper()
	if len(packet) < 28 || packet[0]>>4 != 4 || packet[9] != 17 {
		t.Fatal("expected an IPv4 UDP packet")
	}
	headerLength := int(packet[0]&15) * 4
	if len(packet) < headerLength+8 {
		t.Fatal("truncated UDP packet")
	}
	reply := append([]byte(nil), packet...)
	copy(reply[12:16], packet[16:20])
	copy(reply[16:20], packet[12:16])
	copy(reply[headerLength:headerLength+2], packet[headerLength+2:headerLength+4])
	copy(reply[headerLength+2:headerLength+4], packet[headerLength:headerLength+2])
	reply[headerLength+6], reply[headerLength+7] = 0, 0
	reply[10], reply[11] = 0, 0
	var sum uint32
	for index := 0; index < headerLength; index += 2 {
		sum += uint32(binary.BigEndian.Uint16(reply[index : index+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(reply[10:12], ^uint16(sum))
	return reply
}

type sessionObservedTransport struct {
	wgc.Transport
	done chan struct{}
	once sync.Once
}

func (t *sessionObservedTransport) Close() error {
	err := t.Transport.Close()
	t.once.Do(func() { close(t.done) })
	return err
}

// Wrap a real stack device so Close can be attempted while an inbound Write is
// paused. The burst uses the real stack's bounded output channel to require the
// sender to keep draining even after the WGC connection has closed.
type sessionGatedDevice struct {
	sg.Device
	burst                     net.Conn
	entered, proceed, written chan struct{}
	closeStarted              chan struct{}
	release                   sync.Once
	closeOnce                 sync.Once
	reads, writes             atomic.Uint64
}

func (d *sessionGatedDevice) Read(buffers [][]byte, sizes []int, offset int) (int, error) {
	count, err := d.Device.Read(buffers, sizes, offset)
	d.reads.Add(uint64(count))
	return count, err
}

func (d *sessionGatedDevice) Write(buffers [][]byte, offset int) (int, error) {
	if len(buffers) == 0 || len(buffers[0]) == offset {
		return d.Device.Write(buffers, offset)
	}
	if d.writes.Add(1) != 1 {
		return d.Device.Write(buffers, offset)
	}
	close(d.entered)
	defer close(d.written)
	<-d.proceed
	count, err := d.Device.Write(buffers, offset)
	if err != nil {
		return count, err
	}
	for index := 0; index < 512; index++ {
		if _, err := d.burst.Write([]byte("response")); err != nil {
			return count, err
		}
	}
	return count, nil
}

func (d *sessionGatedDevice) Close() error {
	d.closeOnce.Do(func() { close(d.closeStarted) })
	select {
	case <-d.entered:
		// Avoid actually detaching gVisor in the middle of Write if the
		// ordering regresses; the test observes the premature Close attempt.
		<-d.written
	default:
	}
	return d.Device.Close()
}

func TestSessionCloseWaitsForReceiveAndDrainsOutput(t *testing.T) {
	private, public := testKeys(t)
	remote := newSessionTestPeer(t, public)
	privateKey, _ := parseKey(private)
	remoteKey, _ := parseKey(remote.public)
	remoteAddress := netip.MustParseAddrPort(remote.endpoint)
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	trans := &sessionObservedTransport{Transport: udp.FromUDPConn(socket), done: make(chan struct{})}
	meter := &meteredTransport{Transport: trans}
	conn, err := wgc.NewConn(meter, wgc.Config{
		StaticPrivateKey: &privateKey, DisableStats: true, SharedWriteQueue: true,
		MaxPeers: 1, ReadQueueSize: 128, WriteQueueSize: 128,
		PeerConfigLookup: func(key wgc.Key) (*wgc.PeerConfig, error) {
			if key != remoteKey {
				return nil, nil
			}
			return &wgc.PeerConfig{Addr: remoteAddress, DisableAddrRoaming: true}, nil
		},
	})
	if err != nil {
		_ = trans.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	device, err := sg.NewDevice(sg.DeviceOptions{Context: ctx, MTU: 1280, Address: []netip.Prefix{netip.MustParsePrefix("10.7.0.2/32")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = device.Close() })
	if err := device.Start(); err != nil {
		t.Fatal(err)
	}
	burst, err := device.DialContext(ctx, "udp", M.Socksaddr{Addr: netip.MustParseAddr("10.7.0.1"), Port: 8081})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = burst.Close() })
	gated := &sessionGatedDevice{Device: device, burst: burst, entered: make(chan struct{}), proceed: make(chan struct{}), written: make(chan struct{}), closeStarted: make(chan struct{})}
	entry := &peerDevice{device: gated, key: remoteKey}
	session := &Session{conn: conn, meter: meter, peers: map[string]*peerDevice{"test": entry}, keys: map[wgc.Key]*peerDevice{remoteKey: entry}, ctx: ctx, cancel: cancel, done: make(chan struct{}), publicKey: public}
	// Install the wrapper before either worker starts; replacing a running
	// session's device would itself race with the sender.
	session.senders.Add(1)
	go session.send(entry, 1280)
	session.receiver.Add(1)
	go session.receive()
	t.Cleanup(func() {
		gated.release.Do(func() { close(gated.proceed) })
		// Unblock the burst even if a regressed sender stopped draining.
		_ = device.Close()
		_ = session.Close()
	})
	packet := warmSessionTestPeer(t, burst, remote)
	clientKey, _ := parseKey(public)
	if _, _, err := remote.conn.WriteTo(sessionTestUDPReply(t, packet), clientKey); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gated.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("receiver did not enter the gated device Write")
	}
	if _, _, err := remote.conn.WriteTo(sessionTestUDPReply(t, packet), clientKey); err != nil {
		t.Fatal(err)
	}
	queued := time.NewTicker(time.Millisecond)
	defer queued.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for len(conn.ReadQueue()) == 0 {
		select {
		case <-queued.C:
		case <-deadline.C:
			t.Fatal("second inbound packet did not queue behind the gated Write")
		}
	}
	closed := make(chan struct{})
	go func() { _ = session.Close(); close(closed) }()
	select {
	case <-trans.done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not close the encrypted transport")
	}
	select {
	case <-gated.closeStarted:
		t.Error("device Close started before the in-flight receive completed")
	case <-time.After(100 * time.Millisecond):
	}
	gated.release.Do(func() { close(gated.proceed) })
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not drain stack output and join the receiver")
	}
	if reads := gated.reads.Load(); reads < 257 {
		t.Fatalf("response burst did not exceed the stack output capacity: reads=%d", reads)
	}
	if writes := gated.writes.Load(); writes != 1 {
		t.Fatalf("shutdown injected queued packets after cancellation: writes=%d", writes)
	}
	if err := session.Err(); err != nil {
		t.Fatalf("normal shutdown recorded an error: %v", err)
	}
}

func TestSessionDeniedPeerDoesNotStopAuthorizedPeer(t *testing.T) {
	for _, lookupErr := range []error{nil, errors.New("admission denied")} {
		t.Run(fmt.Sprint(lookupErr), func(t *testing.T) {
			private, public := testKeys(t)
			remote := newSessionTestPeer(t, public)
			_, deniedPublic := testKeys(t)
			remoteKey, _ := parseKey(remote.public)
			deniedKey, _ := parseKey(deniedPublic)
			clientKey, _ := parseKey(public)
			attempts := make(chan struct{}, 8)
			session, err := NewSession(context.Background(), Config{Version: 1, PrivateKey: private, Addresses: []string{"10.7.0.2/32"}, Peers: []Peer{
				{ID: "allowed", PublicKey: remote.public, Endpoint: remote.endpoint},
				{ID: "denied", PublicKey: deniedPublic, Endpoint: "127.0.0.1:9"},
			}}, SessionOptions{PeerConfigLookup: func(key wgc.Key) (*wgc.PeerConfig, error) {
				if key == deniedKey {
					attempts <- struct{}{}
					return nil, lookupErr
				}
				if key != remoteKey {
					return nil, nil
				}
				return &wgc.PeerConfig{Addr: netip.MustParseAddrPort(remote.endpoint), DisableAddrRoaming: true}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			allowed := sessionTestUDP(t, session, "allowed")
			denied := sessionTestUDP(t, session, "denied")
			warmSessionTestPeer(t, allowed, remote)
			for index := 0; index < 3; index++ {
				if _, err := denied.Write([]byte("denied")); err != nil {
					t.Fatal(err)
				}
				select {
				case <-attempts:
				case <-session.Done():
					t.Fatalf("denied peer stopped the session: %v", session.Err())
				case <-time.After(3 * time.Second):
					t.Fatal("denied peer's sender did not retry admission")
				}
				payload := []byte(fmt.Sprintf("authorized-%d", index))
				if _, err := allowed.Write(payload); err != nil {
					t.Fatal(err)
				}
				deadline := time.NewTimer(3 * time.Second)
				var packet []byte
				for packet == nil {
					select {
					case candidate := <-remote.packets:
						if bytes.HasSuffix(candidate, payload) {
							packet = candidate
						}
					case <-session.Done():
						t.Fatalf("denied peer stopped authorized traffic: %v", session.Err())
					case <-deadline.C:
						t.Fatal("authorized peer stopped sending")
					}
				}
				deadline.Stop()
				if _, _, err := remote.conn.WriteTo(sessionTestUDPReply(t, packet), clientKey); err != nil {
					t.Fatal(err)
				}
				_ = allowed.SetReadDeadline(time.Now().Add(3 * time.Second))
				reply := make([]byte, 64)
				n, err := allowed.Read(reply)
				if err != nil || !bytes.Equal(reply[:n], payload) {
					t.Fatalf("authorized round trip after denial: payload=%q err=%v", reply[:n], err)
				}
			}
			if err := session.Err(); err != nil || session.Status().State != "running" {
				t.Fatalf("peer denial changed session health: %v", err)
			}
		})
	}
}

type sessionFailingTransport struct {
	fail, closed chan struct{}
	once         sync.Once
}

func (t *sessionFailingTransport) ReadFrom([]byte) (int, netip.AddrPort, error) {
	select {
	case <-t.fail:
		return 0, netip.AddrPort{}, errors.New("fatal receive")
	case <-t.closed:
		return 0, netip.AddrPort{}, net.ErrClosed
	}
}
func (t *sessionFailingTransport) WriteTo(packet []byte, _ netip.AddrPort) (int, error) {
	return len(packet), nil
}
func (t *sessionFailingTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

func TestSessionFatalReceiveStopsSession(t *testing.T) {
	trans := &sessionFailingTransport{fail: make(chan struct{}), closed: make(chan struct{})}
	registry := coreTransport.NewRegistry()
	if err := registry.RegisterFactory("test-fatal", func(context.Context, json.RawMessage) (wgc.Transport, error) { return trans, nil }); err != nil {
		t.Fatal(err)
	}
	config := testConfig(t)
	config.Transport.Type = "test-fatal"
	session, err := NewSession(context.Background(), config, SessionOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	close(trans.fail)
	select {
	case <-session.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("fatal receive did not stop the session")
	}
	if !errors.Is(session.Err(), net.ErrClosed) || session.Status().State != "stopped" {
		t.Fatalf("fatal receive was not recorded: err=%v status=%+v", session.Err(), session.Status())
	}
}

type sessionReadDevice struct {
	sg.Device
	read func([][]byte, []int, int) (int, error)
}

func (d *sessionReadDevice) Read(buffers [][]byte, sizes []int, offset int) (int, error) {
	return d.read(buffers, sizes, offset)
}

func TestSessionBlockedPeerDoesNotStopAuthorizedPeer(t *testing.T) {
	private, public := testKeys(t)
	allowedRemote := newSessionTestPeer(t, public)
	blockedRemote := newSessionTestPeer(t, public)
	session, err := NewSession(context.Background(), Config{Version: 1, PrivateKey: private, Addresses: []string{"10.7.0.2/32"}, Peers: []Peer{
		{ID: "allowed", PublicKey: allowedRemote.public, Endpoint: allowedRemote.endpoint},
		{ID: "blocked", PublicKey: blockedRemote.public, Endpoint: blockedRemote.endpoint},
	}}, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	allowed := sessionTestUDP(t, session, "allowed")
	blocked := sessionTestUDP(t, session, "blocked")
	warmSessionTestPeer(t, allowed, allowedRemote)
	packet := warmSessionTestPeer(t, blocked, blockedRemote)
	key, _ := parseKey(blockedRemote.public)
	peer, ok := session.conn.Peer(key)
	if !ok || !peer.Block() {
		t.Fatal("configured peer was not blocked")
	}
	processed := make(chan struct{})
	read := false
	device := &sessionReadDevice{read: func(buffers [][]byte, sizes []int, offset int) (int, error) {
		if !read {
			read = true
			sizes[0] = copy(buffers[0][offset:], packet)
			return 1, nil
		}
		// The next Read proves send processed the real WGC WriteTo result.
		close(processed)
		<-session.ctx.Done()
		return 0, session.ctx.Err()
	}}
	// Drive another sender through the real shared connection without replacing
	// any running stack device. Cancellation releases its final Read.
	session.senders.Add(1)
	go session.send(&peerDevice{device: device, key: key}, 1280)
	select {
	case <-processed:
	case <-session.Done():
		t.Fatalf("blocked peer stopped the session: %v", session.Err())
	case <-time.After(3 * time.Second):
		t.Fatal("sender did not process the blocked peer's packet")
	}
	if err := session.Err(); err != nil || session.Status().State != "running" {
		t.Fatalf("blocked peer changed session health: %v", err)
	}
	clientKey, _ := parseKey(public)
	roundTrip := func(conn net.Conn, remote *sessionTestPeer, payload []byte) {
		t.Helper()
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		var received []byte
		for received == nil {
			select {
			case candidate := <-remote.packets:
				if bytes.HasSuffix(candidate, payload) {
					received = candidate
				}
			case <-session.Done():
				t.Fatalf("blocked peer stopped traffic: %v", session.Err())
			case <-deadline.C:
				t.Fatalf("peer did not receive %q", payload)
			}
		}
		if _, _, err := remote.conn.WriteTo(sessionTestUDPReply(t, received), clientKey); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		reply := make([]byte, 64)
		n, err := conn.Read(reply)
		if err != nil || !bytes.Equal(reply[:n], payload) {
			t.Fatalf("round trip: payload=%q want=%q err=%v", reply[:n], payload, err)
		}
	}
	roundTrip(allowed, allowedRemote, []byte("authorized after block"))
	if !peer.Unblock() {
		t.Fatal("blocked peer did not unblock")
	}
	roundTrip(blocked, blockedRemote, []byte("recovered after unblock"))
	if err := session.Err(); err != nil || session.Status().State != "running" {
		t.Fatalf("peer recovery changed session health: %v", err)
	}
}

func TestSessionTerminalDeviceReadErrorStopsSession(t *testing.T) {
	session, err := NewSession(context.Background(), testConfig(t), SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	fatal := errors.New("fatal device read")
	device := &sessionReadDevice{read: func([][]byte, []int, int) (int, error) { return 0, fatal }}
	session.senders.Add(1)
	go session.send(&peerDevice{device: device}, 1280)
	select {
	case <-session.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("terminal device read error did not stop the session")
	}
	if !errors.Is(session.Err(), fatal) || session.Status().State != "stopped" {
		t.Fatalf("terminal device read error was not preserved: %v", session.Err())
	}
}

func TestSessionStatusDuringShutdown(t *testing.T) {
	config := testConfig(t)
	session, err := NewSession(context.Background(), config, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	initial := session.Status()
	changed := initial
	changed.PublicKey, changed.State = "changed", "changed"
	if session.Status() != initial {
		t.Fatal("modifying a status snapshot changed the session")
	}
	start := make(chan struct{})
	results := make(chan error, 8)
	for index := 0; index < cap(results); index++ {
		go func() {
			<-start
			for count := 0; count < 1000; count++ {
				status := session.Status()
				if status.PublicKey != initial.PublicKey || status.CoreVersion != SingBoxVersion || (status.State != "running" && status.State != "stopped") {
					results <- fmt.Errorf("invalid concurrent status: %+v", status)
					return
				}
			}
			<-session.Done()
			if status := session.Status(); status.State != "stopped" || status.PublicKey != initial.PublicKey {
				results <- fmt.Errorf("invalid stopped status: %+v", status)
				return
			}
			results <- nil
		}()
	}
	close(start)
	_ = session.Close()
	for index := 0; index < cap(results); index++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if initial.State != "running" {
		t.Fatal("shutdown changed an earlier status snapshot")
	}
}

type sessionGatedTransport struct {
	wgc.Transport
	gate          atomic.Bool
	entered, done chan struct{}
	enter, close  sync.Once
}

func (t *sessionGatedTransport) WriteTo(packet []byte, remote netip.AddrPort) (int, error) {
	if t.gate.Load() && len(packet) >= 4 && binary.LittleEndian.Uint32(packet) == 4 {
		t.enter.Do(func() { close(t.entered) })
		<-t.done
		return 0, net.ErrClosed
	}
	return t.Transport.WriteTo(packet, remote)
}
func (t *sessionGatedTransport) Close() error {
	t.close.Do(func() { close(t.done) })
	return t.Transport.Close()
}

func TestSessionSharedQueueBackpressureAndClose(t *testing.T) {
	private, public := testKeys(t)
	remote := newSessionTestPeer(t, public)
	registry := coreTransport.NewRegistry()
	var gated *sessionGatedTransport
	if err := registry.RegisterWrapper("test-gated", func(_ context.Context, base wgc.Transport, _ json.RawMessage) (wgc.Transport, error) {
		gated = &sessionGatedTransport{Transport: base, entered: make(chan struct{}), done: make(chan struct{})}
		return gated, nil
	}); err != nil {
		t.Fatal(err)
	}
	session, err := NewSession(context.Background(), Config{Version: 1, PrivateKey: private, Addresses: []string{"10.7.0.2/32"}, Peers: []Peer{{ID: "test", PublicKey: remote.public, Endpoint: remote.endpoint}}, Transport: coreTransport.Spec{Type: "test-gated"}}, SessionOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	conn := sessionTestUDP(t, session, "test")
	warmSessionTestPeer(t, conn, remote)
	gated.gate.Store(true)
	if _, err := conn.Write([]byte("gate")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gated.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("encrypted transport did not reach the write gate")
	}
	written := make(chan error, 1)
	go func() {
		// This exceeds the shared WGC queue (128) plus stack output (256),
		// but fits the accidental private PeerWriter queue (1024).
		for index := 0; index < 512; index++ {
			if _, err := conn.Write([]byte("queued")); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	select {
	case err := <-written:
		t.Fatalf("bounded shared queue did not apply backpressure: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	closed := make(chan struct{})
	go func() { _ = session.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock shared-queue backpressure")
	}
	select {
	case <-written:
		// Cancellation drains queued output, so the producer may finish its
		// writes before device.Close or observe the closed stack instead.
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not unblock the stack producer")
	}
}
