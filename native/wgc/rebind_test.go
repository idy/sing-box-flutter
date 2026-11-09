//go:build with_gvisor

package wgc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	wireguard "github.com/idy/wgc"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/service"
)

type closingTransport struct {
	done chan struct{}
	once sync.Once
}

func (t *closingTransport) ReadFrom([]byte) (int, netip.AddrPort, error) {
	<-t.done
	return 0, netip.AddrPort{}, net.ErrClosed
}
func (t *closingTransport) WriteTo(data []byte, _ netip.AddrPort) (int, error) {
	select {
	case <-t.done:
		return 0, net.ErrClosed
	default:
		return len(data), nil
	}
}
func (t *closingTransport) Close() error { t.once.Do(func() { close(t.done) }); return nil }

func TestCloseCancelsPendingInterfaceRebind(t *testing.T) {
	var calls atomic.Int32
	started, stopped := make(chan struct{}), make(chan struct{})
	registerTestTransport(t, "blocked-rebind-test", func(ctx context.Context, _ netip.AddrPort, _ json.RawMessage) (wireguard.Transport, error) {
		call := calls.Add(1)
		if call == 1 {
			return &closingTransport{done: make(chan struct{})}, nil
		}
		if call == 2 {
			close(started)
		}
		<-ctx.Done()
		if call == 2 {
			close(stopped)
		}
		return nil, ctx.Err()
	})
	options := testOptions(t)
	options.Transport = "blocked-rebind-test"
	created, err := NewEndpoint(context.Background(), nil, nil, "", options)
	if err != nil {
		t.Fatal(err)
	}
	ep := created.(*Endpoint)
	scope := adapter.NewScope(context.Background(), nil)
	defer scope.Close()
	for _, stage := range []adapter.StartStage{adapter.StartStateInitialize, adapter.StartStateStart} {
		if err := ep.Start(stage, scope); err != nil {
			t.Fatal(err)
		}
	}
	ep.InterfaceUpdated(context.Background())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("interface callback did not reopen the socket")
	}
	closed := make(chan struct{})
	go func() { _ = ep.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel and join the rebind worker")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("transport factory was left running after Close")
	}
	before := calls.Load()
	ep.InterfaceUpdated(context.Background())
	if calls.Load() != before {
		t.Fatal("closed endpoint reopened its transport")
	}
}

func TestCloseDuringSessionRecovery(t *testing.T) {
	for _, pendingFactory := range []bool{false, true} {
		name := "backoff"
		if pendingFactory {
			name = "transport-startup"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			var original, failed *closingTransport
			attempts := make(chan time.Time, 4)
			stopped := make(chan struct{})
			transportName := "session-recovery-" + name
			registerTestTransport(t, transportName, func(ctx context.Context, _ netip.AddrPort, _ json.RawMessage) (wireguard.Transport, error) {
				call := calls.Add(1)
				attempts <- time.Now()
				switch call {
				case 1:
					original = &closingTransport{done: make(chan struct{})}
					return original, nil
				case 2:
					failed = &closingTransport{done: make(chan struct{})}
					return failed, errors.New("injected reopen failure")
				default:
					<-ctx.Done()
					close(stopped)
					return nil, ctx.Err()
				}
			})
			o := testOptions(t)
			o.Transport = transportName
			created, err := NewEndpoint(context.Background(), nil, nil, "", o)
			if err != nil {
				t.Fatal(err)
			}
			ep := created.(*Endpoint)
			scope := adapter.NewScope(context.Background(), nil)
			startTestEndpoint(t, ep, scope)
			oldCtx := ep.runCtx
			// An unexpected device close models a fatal Read on the live stack.
			_ = ep.device.Close()
			select {
			case <-oldCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("failed stack did not schedule recovery")
			}
			if pendingFactory {
				var last time.Time
				for range 3 {
					select {
					case attempt := <-attempts:
						if !last.IsZero() && attempt.Sub(last) < 900*time.Millisecond {
							t.Fatal("recovery attempts were not rate limited")
						}
						last = attempt
					case <-time.After(5 * time.Second):
						t.Fatal("recovery stopped retrying after a transport startup failure")
					}
				}
			}
			closeTestEndpoint(t, ep)
			select {
			case <-original.done:
			default:
				t.Fatal("failed session leaked its original transport")
			}
			if pendingFactory {
				select {
				case <-stopped:
				default:
					t.Fatal("Close did not cancel and join pending recovery startup")
				}
				select {
				case <-failed.done:
				default:
					t.Fatal("failed recovery startup leaked its partial transport")
				}
				if calls.Load() != 3 {
					t.Fatal("Close started another recovery attempt")
				}
			} else if calls.Load() != 1 {
				t.Fatal("Close did not cancel the recovery backoff")
			}
		})
	}
}

type protectionManager struct {
	adapter.NetworkManager
	protect control.Func
}

func (m protectionManager) ProtectFunc() control.Func { return m.protect }

func protectionContext(protect control.Func) context.Context {
	return service.ContextWith[adapter.NetworkManager](context.Background(), protectionManager{protect: protect})
}

func forcedAndroidUDP(ctx context.Context, peer netip.AddrPort, options json.RawMessage) (wireguard.Transport, error) {
	return newUDPTransport(ctx, peer, options, true)
}

func checkUDPProtection(t *testing.T, factory TransportFactory) {
	t.Helper()
	peer := netip.MustParseAddrPort("127.0.0.1:51820")
	for name, ctx := range map[string]context.Context{"missing-manager": context.Background(), "missing-hook": protectionContext(nil)} {
		t.Run(name, func(t *testing.T) {
			transport, err := factory(ctx, peer, nil)
			if transport != nil {
				_ = transport.Close()
			}
			if err == nil || transport != nil {
				t.Fatal("unprotected socket was accepted")
			}
		})
	}
	denied := errors.New("protection denied")
	var failures atomic.Int32
	transport, err := factory(protectionContext(func(string, string, syscall.RawConn) error {
		failures.Add(1)
		return denied
	}), peer, nil)
	if transport != nil {
		_ = transport.Close()
	}
	if transport != nil || !errors.Is(err, denied) || failures.Load() != 1 {
		t.Fatal("socket protection failure was not propagated")
	}
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var protected atomic.Int32
	transport, err = factory(protectionContext(func(network, _ string, raw syscall.RawConn) error {
		if network != "udp4" {
			return errors.New("unexpected socket family")
		}
		var socketErr error
		err := raw.Control(func(fd uintptr) {
			kind, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_TYPE)
			if err != nil {
				socketErr = err
				return
			}
			if kind != syscall.SOCK_DGRAM {
				socketErr = errors.New("protection received a non-UDP descriptor")
			}
		})
		if err == nil && socketErr == nil {
			protected.Add(1)
		}
		return errors.Join(err, socketErr)
	}), server.LocalAddr().(*net.UDPAddr).AddrPort(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	if protected.Load() != 1 {
		t.Fatal("socket was not protected before use")
	}
	if _, err := transport.WriteTo([]byte("protected"), server.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
		t.Fatal(err)
	}
	_ = server.SetReadDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 64)
	n, from, err := server.ReadFromUDP(buffer)
	if err != nil || string(buffer[:n]) != "protected" {
		t.Fatalf("protected UDP send failed: %v", err)
	}
	if _, err := server.WriteToUDP([]byte("reply"), from); err != nil {
		t.Fatal(err)
	}
	received := make(chan error, 1)
	go func() {
		data := make([]byte, 64)
		n, _, err := transport.ReadFrom(data)
		if err == nil && string(data[:n]) != "reply" {
			err = errors.New("protected UDP receive changed payload")
		}
		received <- err
	}()
	select {
	case err := <-received:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("protected UDP receive did not complete")
	}
}

func checkProtectedReopen(t *testing.T, factory TransportFactory) {
	t.Helper()
	var attempts atomic.Int32
	var denyReopen atomic.Bool
	failed := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(protectionContext(func(_ string, _ string, raw syscall.RawConn) error {
		attempts.Add(1)
		if denyReopen.Load() {
			select {
			case failed <- struct{}{}:
			default:
			}
			return errors.New("reopen protection denied")
		}
		return raw.Control(func(uintptr) {})
	}), 10*time.Second)
	defer cancel()
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	registerTestTransport(t, "protected-reopen-test", factory)
	options := testOptions(t)
	options.Transport = "protected-reopen-test"
	options.PeerEndpoint = server.LocalAddr().(*net.UDPAddr).AddrPort()
	created, err := NewEndpoint(ctx, nil, nil, "", options)
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
	original, changed := ep.binding.snapshot()
	originalConnection, originalDevice := ep.conn, ep.device
	denyReopen.Store(true)
	ep.InterfaceUpdated(ctx)
	select {
	case <-failed:
	case <-ctx.Done():
		t.Fatal("reopen did not call protection")
	}
	if current, _ := ep.binding.snapshot(); current != original {
		t.Fatal("failed protection replaced the working socket")
	}
	if _, err := original.WriteTo([]byte("still-open"), options.PeerEndpoint); err != nil {
		t.Fatal(err)
	}
	readPayload := func(want string) {
		t.Helper()
		_ = server.SetReadDeadline(time.Now().Add(3 * time.Second))
		buffer := make([]byte, 2048)
		for {
			n, _, err := server.ReadFromUDP(buffer)
			if err != nil {
				t.Fatalf("protected socket did not send %q: %v", want, err)
			}
			if string(buffer[:n]) == want {
				return
			}
		}
	}
	readPayload("still-open")
	denyReopen.Store(false)
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("protected reopen never recovered")
	}
	current, _ := ep.binding.snapshot()
	if current == original || attempts.Load() < 3 {
		t.Fatal("replacement socket did not repeat protection")
	}
	if ep.conn != originalConnection || ep.device != originalDevice {
		t.Fatal("reopen replaced the WGC session or stack")
	}
	if _, err := current.WriteTo([]byte("reopened"), options.PeerEndpoint); err != nil {
		t.Fatal(err)
	}
	readPayload("reopened")
	_ = ep.Close()
	if _, err := original.WriteTo([]byte("closed"), options.PeerEndpoint); err == nil {
		t.Fatal("old protected socket was not closed")
	}
}

func TestUDPProtectionInjectedPath(t *testing.T) { checkUDPProtection(t, forcedAndroidUDP) }
func TestUDPProtectionOnReopen(t *testing.T)     { checkProtectedReopen(t, forcedAndroidUDP) }

func TestAndroidUDPProtectionSmoke(t *testing.T) {
	if runtime.GOOS != "android" {
		t.Skip("Android runtime smoke")
	}
	t.Run("socket", func(t *testing.T) { checkUDPProtection(t, NewUDPTransport) })
	t.Run("reopen", func(t *testing.T) { checkProtectedReopen(t, NewUDPTransport) })
}
