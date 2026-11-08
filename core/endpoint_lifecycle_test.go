//go:build with_gvisor

package core

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/idy/sing-box-flutter/core/transport"
	wgc "github.com/idy/wgc"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

func startTestEndpoint(t *testing.T, ctx context.Context, options EndpointOptions) (*Endpoint, *adapter.Scope) {
	t.Helper()
	ep := newTestEndpoint(t, ctx, options)
	scope := adapter.NewScope(ctx, nil)
	t.Cleanup(func() { _ = scope.Close() })
	for _, stage := range adapter.ListStartStages {
		if err := ep.Start(stage, scope); err != nil {
			t.Fatal(err)
		}
	}
	return ep, scope
}

func TestEndpointScopeCloseCancelsConcurrentTraffic(t *testing.T) {
	ep, scope := startTestEndpoint(t, context.Background(), EndpointOptions{Configuration: testConfig(t)})
	destination := M.ParseSocksaddr("10.7.0.1:8080")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{}, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			started <- struct{}{}
			for range 32 {
				conn, err := ep.DialContext(ctx, "udp", destination)
				if err != nil {
					return
				}
				_ = conn.Close()
				packet, err := ep.ListenPacket(ctx, destination)
				if err != nil {
					return
				}
				_ = packet.Close()
			}
		}()
	}
	for range 8 {
		<-started
	}
	// A real TCP handshake to an absent remote stays pending until shutdown.
	tcpResult := make(chan error, 1)
	go func() {
		conn, err := ep.DialContext(ctx, "tcp", destination)
		if conn != nil {
			_ = conn.Close()
		}
		tcpResult <- err
	}()
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent UDP operations did not stop")
	}
	select {
	case err := <-tcpResult:
		if err == nil {
			t.Fatal("stopped TCP dial succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel TCP dial")
	}
	select {
	case <-ep.session.Done():
	default:
		t.Fatal("scope cleanup returned before session stopped")
	}
	if _, err := ep.DialContext(ctx, "udp", destination); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("new traffic did not fail closed: %v", err)
	}
	if _, err := ep.ListenPacket(ctx, destination); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("new packet socket did not fail closed: %v", err)
	}
	if err := ep.Start(adapter.StartStateStart, scope); !errors.Is(err, net.ErrClosed) {
		t.Fatal("closed endpoint restarted")
	}
}

type endpointBlockingDNS struct {
	adapter.DNSRouter
	entered chan struct{}
}

func (r *endpointBlockingDNS) Lookup(ctx context.Context, _ string, _ adapter.DNSQueryOptions) ([]netip.Addr, error) {
	close(r.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestEndpointShutdownCancelsDNSLookup(t *testing.T) {
	for _, operation := range []string{"tcp", "udp"} {
		t.Run(operation, func(t *testing.T) {
			router := &endpointBlockingDNS{entered: make(chan struct{})}
			ctx := service.ContextWith[adapter.DNSRouter](context.Background(), router)
			ctx = service.ContextWith[adapter.DNSTransportManager](ctx, &outboundDNSManager{transport: &outboundDNSTransport{}})
			ep, scope := startTestEndpoint(t, ctx, EndpointOptions{Configuration: testConfig(t), Resolver: "outbound-test-resolver"})
			result := make(chan error, 1)
			go func() {
				if operation == "tcp" {
					conn, err := ep.DialContext(context.Background(), "tcp", outboundTestDomain)
					if conn != nil {
						_ = conn.Close()
					}
					result <- err
				} else {
					conn, err := ep.ListenPacket(context.Background(), outboundTestDomain)
					if conn != nil {
						_ = conn.Close()
					}
					result <- err
				}
			}()
			select {
			case <-router.entered:
			case <-time.After(time.Second):
				t.Fatal("operation did not enter DNS lookup")
			}
			_ = scope.Close()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("DNS cancellation lost: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("shutdown left DNS lookup running")
			}
		})
	}
}

func TestEndpointSessionFailureRejectsNewTrafficBeforeDNS(t *testing.T) {
	trans := &sessionFailingTransport{fail: make(chan struct{}), closed: make(chan struct{})}
	registry := transport.NewRegistry()
	if err := registry.RegisterFactory("endpoint-failure", func(context.Context, json.RawMessage) (wgc.Transport, error) { return trans, nil }); err != nil {
		t.Fatal(err)
	}
	config := testConfig(t)
	config.Transport.Type = "endpoint-failure"
	ctx := WithSessionOptions(context.Background(), SessionOptions{Registry: registry})
	ep, _ := startTestEndpoint(t, ctx, EndpointOptions{Configuration: config})
	close(trans.fail)
	select {
	case <-ep.session.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("failed transport did not stop the session")
	}
	// No DNS services are registered. Failure must be the stopped session,
	// before domain lookup or any new transport/device operation.
	if _, err := ep.DialContext(ctx, "tcp", outboundTestDomain); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("failed session accepted a dial or queried DNS: %v", err)
	}
	if _, err := ep.ListenPacket(ctx, outboundTestDomain); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("failed session accepted a packet socket or queried DNS: %v", err)
	}
}
