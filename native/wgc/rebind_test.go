//go:build with_gvisor

package wgc

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wireguard "github.com/idy/wgc"
	"github.com/sagernet/sing-box/adapter"
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
