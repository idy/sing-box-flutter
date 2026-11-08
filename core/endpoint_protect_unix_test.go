//go:build with_gvisor && unix

package core

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/idy/sing-box-flutter/core/transport"
	wgc "github.com/idy/wgc"
	udp "github.com/idy/wgc/transport"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"
	"golang.org/x/sys/unix"
)

type endpointTestPlatform struct {
	adapter.PlatformInterface
	protect transport.SocketProtector
}

func (*endpointTestPlatform) UsePlatformAutoDetectInterfaceControl() bool { return true }
func (p *endpointTestPlatform) AutoDetectInterfaceControl(fd int) error   { return p.protect(fd) }

func TestEndpointPlatformProtectsBuiltinAndExtensionSockets(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, reject := range []bool{false, true} {
			name := map[bool]string{false: "builtin", true: "factory_and_wrapper"}[custom] + map[bool]string{false: "_accepted", true: "_rejected"}[reject]
			t.Run(name, func(t *testing.T) {
				var mu sync.Mutex
				var fds []int
				platform := &endpointTestPlatform{protect: func(fd int) error {
					mu.Lock()
					fds = append(fds, fd)
					mu.Unlock()
					address, err := unix.Getsockname(fd)
					if err != nil {
						return err
					}
					if address.(*unix.SockaddrInet4).Port != 0 {
						t.Error("endpoint socket was bound before platform protection")
					}
					if reject {
						return errors.New("private-platform-error")
					}
					return nil
				}}
				ctx := service.ContextWith[adapter.PlatformInterface](context.Background(), platform)
				registry := transport.NewRegistry()
				config := testConfig(t)
				config.Transport.Options = json.RawMessage(`{"listen":"127.0.0.1:0"}`)
				if custom {
					open := func(ctx context.Context) (net.PacketConn, error) {
						if !transport.HasSocketProtector(ctx) {
							t.Error("extension lost platform protector")
						}
						listener := net.ListenConfig{Control: transport.SocketControl(ctx)}
						return listener.ListenPacket(ctx, "udp4", "127.0.0.1:0")
					}
					if err := registry.RegisterFactory("endpoint-custom", func(ctx context.Context, _ json.RawMessage) (wgc.Transport, error) {
						conn, err := open(ctx)
						if err != nil {
							return nil, err
						}
						return udp.FromUDPConn(conn.(*net.UDPConn)), nil
					}); err != nil {
						t.Fatal(err)
					}
					if err := registry.RegisterWrapper("endpoint-wrapper", func(ctx context.Context, base wgc.Transport, _ json.RawMessage) (wgc.Transport, error) {
						auxiliary, err := open(ctx)
						if err != nil {
							return nil, err
						}
						_ = auxiliary.Close()
						return base, nil
					}); err != nil {
						t.Fatal(err)
					}
					config.Transport = transport.Spec{Type: "endpoint-wrapper", Base: &transport.Spec{Type: "endpoint-custom"}}
				}
				ctx = WithSessionOptions(ctx, SessionOptions{Registry: registry})
				ep := newTestEndpoint(t, ctx, EndpointOptions{Configuration: config})
				if len(fds) != 0 {
					t.Fatal("constructor touched the platform socket API")
				}
				scope := adapter.NewScope(ctx, nil)
				err := ep.Start(adapter.StartStateStart, scope)
				if reject {
					if err == nil || strings.Contains(err.Error(), "private-platform") {
						t.Fatalf("protection rejection lost or leaked: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				_ = scope.Close()
				mu.Lock()
				defer mu.Unlock()
				wantCalls := 1
				if custom && !reject {
					wantCalls = 2
				}
				if len(fds) != wantCalls {
					t.Fatalf("protected sockets = %d, want %d", len(fds), wantCalls)
				}
				for _, fd := range fds {
					if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
						t.Fatalf("scope cleanup leaked outer socket: %v", err)
					}
				}
			})
		}
	}
}
