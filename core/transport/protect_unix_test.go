//go:build unix

package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"testing"

	wgc "github.com/idy/wgc"
	udp "github.com/idy/wgc/transport"
	"golang.org/x/sys/unix"
)

func assertClosedSocket(t *testing.T, fd int) {
	t.Helper()
	if fd < 0 {
		t.Fatal("protector was not called")
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
		t.Fatalf("socket descriptor %d is still open: %v", fd, err)
	}
}

func TestUDPProtectionBeforeBindAndSocketOwnership(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[reject], func(t *testing.T) {
			fd := -1
			var calls int
			ctx := WithSocketProtector(context.Background(), func(socket int) error {
				fd, calls = socket, calls+1
				address, err := unix.Getsockname(socket)
				if err != nil {
					return err
				}
				if address.(*unix.SockaddrInet4).Port != 0 {
					t.Error("UDP socket was bound before protection")
				}
				if reject {
					return errors.New("private-host-error")
				}
				return nil
			})
			conn, err := NewRegistry().Open(ctx, Spec{Options: json.RawMessage(`{"listen":"127.0.0.1:0"}`)})
			if calls != 1 {
				t.Fatalf("protector calls = %d, want 1", calls)
			}
			if reject {
				if conn != nil || err == nil || err.Error() != "cannot open UDP transport" {
					t.Fatalf("protection failure was not redacted/closed: %v", err)
				}
				assertClosedSocket(t, fd)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
				t.Fatalf("protector closed its borrowed fd: %v", err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			assertClosedSocket(t, fd)
		})
	}
}

func TestCustomFactoryAndWrapperReceiveProtection(t *testing.T) {
	registry := NewRegistry()
	var calls atomic.Int32
	ctx := WithSocketProtector(context.Background(), func(fd int) error {
		calls.Add(1)
		_, err := unix.Getsockname(fd)
		return err
	})
	open := func(ctx context.Context) (net.PacketConn, error) {
		listener := net.ListenConfig{Control: SocketControl(ctx)}
		return listener.ListenPacket(ctx, "udp4", "127.0.0.1:0")
	}
	if err := registry.RegisterFactory("custom", func(ctx context.Context, _ json.RawMessage) (wgc.Transport, error) {
		conn, err := open(ctx)
		if err != nil {
			return nil, err
		}
		return udp.FromUDPConn(conn.(*net.UDPConn)), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterWrapper("extra-socket", func(ctx context.Context, base wgc.Transport, _ json.RawMessage) (wgc.Transport, error) {
		conn, err := open(ctx)
		if err != nil {
			return nil, err
		}
		_ = conn.Close()
		return base, nil
	}); err != nil {
		t.Fatal(err)
	}
	conn, err := registry.Open(ctx, Spec{Type: "extra-socket", Base: &Spec{Type: "custom"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if calls.Load() != 2 {
		t.Fatalf("factory/wrapper sockets did not both protect: %d", calls.Load())
	}
}

type brokenRawConn struct{}

func (brokenRawConn) Control(func(uintptr)) error    { return errors.New("private-raw-conn-error") }
func (brokenRawConn) Read(func(uintptr) bool) error  { panic("unexpected Read") }
func (brokenRawConn) Write(func(uintptr) bool) error { panic("unexpected Write") }

var _ syscall.RawConn = brokenRawConn{}

func TestSocketControlFailsClosedOnRawConnErrorAndCancellation(t *testing.T) {
	var calls int
	ctx := WithSocketProtector(context.Background(), func(int) error { calls++; return nil })
	if err := SocketControl(ctx)("udp", "unused", brokenRawConn{}); !errors.Is(err, ErrSocketProtection) {
		t.Fatalf("raw error not redacted: %v", err)
	}
	if err := SocketControl(ctx)("udp", "unused", nil); !errors.Is(err, ErrSocketProtection) {
		t.Fatalf("nil raw conn accepted: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := ProtectSocket(canceled, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled protection accepted: %v", err)
	}
	if err := ProtectSocket(ctx, -1); !errors.Is(err, ErrSocketProtection) {
		t.Fatalf("invalid fd accepted: %v", err)
	}
	if calls != 0 {
		t.Fatal("invalid/canceled control reached the protector")
	}
}
