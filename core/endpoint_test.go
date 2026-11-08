package core

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idy/sing-box-flutter/core/transport"
	wgc "github.com/idy/wgc"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/option"
	singJSON "github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

func newTestEndpoint(t *testing.T, ctx context.Context, options EndpointOptions) *Endpoint {
	t.Helper()
	created, err := NewEndpoint(ctx, nil, nil, "test-wgc", options)
	if err != nil {
		t.Fatal(err)
	}
	ep := created.(*Endpoint)
	t.Cleanup(func() { _ = ep.Close() })
	return ep
}

func TestEndpointRegistryConstructionIsOffline(t *testing.T) {
	var opens, lookups, protects atomic.Int32
	registry := transport.NewRegistry()
	if err := registry.RegisterFactory("offline-test", func(context.Context, json.RawMessage) (wgc.Transport, error) {
		opens.Add(1)
		return nil, errors.New("transport must not open during construction")
	}); err != nil {
		t.Fatal(err)
	}
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		lookups.Add(1)
		return nil, errors.New("DNS must not run during construction")
	}}
	t.Cleanup(func() { net.DefaultResolver = previous })
	ctx := WithSessionOptions(context.Background(), SessionOptions{Registry: registry})
	ctx = transport.WithSocketProtector(ctx, func(int) error { protects.Add(1); return nil })
	endpoints := endpoint.NewRegistry()
	RegisterEndpoint(endpoints)
	ctx = service.ContextWith[option.EndpointOptionsRegistry](ctx, endpoints)
	config := testConfig(t)
	config.Transport.Type = "offline-test"
	raw, err := json.Marshal(map[string]any{"type": TypeWGC, "tag": "offline", "configuration": config})
	if err != nil {
		t.Fatal(err)
	}
	var decoded option.Endpoint
	if err := singJSON.UnmarshalContext(ctx, raw, &decoded); err != nil {
		t.Fatal(err)
	}
	created, err := endpoints.Create(ctx, nil, nil, decoded.Tag, decoded.Type, decoded.Options)
	if err != nil {
		t.Fatal(err)
	}
	ep := created.(*Endpoint)
	t.Cleanup(func() { _ = ep.Close() })
	scope := adapter.NewScope(ctx, nil)
	if err := ep.Start(adapter.StartStateInitialize, scope); err != nil {
		t.Fatal(err)
	}
	if ep.Type() != "wgc" || ep.Tag() != "offline" || ep.peer != "exit" || ep.resolver != "dns-remote" || ep.session != nil {
		t.Fatal("registry identity, defaults or offline construction changed")
	}
	if opens.Load() != 0 || lookups.Load() != 0 || protects.Load() != 0 {
		t.Fatalf("construction performed IO: opens=%d lookups=%d protects=%d", opens.Load(), lookups.Load(), protects.Load())
	}
	if conn, err := ep.DialContext(ctx, "tcp", M.ParseSocksaddr("10.7.0.1:80")); conn != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatal("endpoint accepted traffic before Start")
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointOptionsRejectBadConfigurationOffline(t *testing.T) {
	const secret = "private-endpoint-test-value"
	for _, test := range []struct {
		name   string
		change func(*EndpointOptions)
	}{
		{"missing configuration", func(o *EndpointOptions) { o.Configuration = Config{} }},
		{"invalid key", func(o *EndpointOptions) { o.Configuration.PrivateKey = secret }},
		{"unknown peer", func(o *EndpointOptions) { o.Peer = secret }},
		{"unknown transport", func(o *EndpointOptions) { o.Configuration.Transport.Type = secret }},
		{"bad UDP options", func(o *EndpointOptions) {
			o.Configuration.Transport.Options = json.RawMessage(`{"listen":"` + secret + `"}`)
		}},
		{"nested sing-box", func(o *EndpointOptions) { o.Configuration.SingBox = json.RawMessage(`{"password":"` + secret + `"}`) }},
		{"nested null", func(o *EndpointOptions) { o.Configuration.SingBox = json.RawMessage(`null`) }},
		{"too large", func(o *EndpointOptions) {
			o.Configuration.Transport.Options, _ = json.Marshal(strings.Repeat(secret, MaxConfigBytes))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := EndpointOptions{Configuration: testConfig(t)}
			test.change(&options)
			created, err := NewEndpoint(context.Background(), nil, nil, "", options)
			if created != nil || err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("invalid config accepted or leaked: %v", err)
			}
		})
	}
	configJSON, _ := json.Marshal(testConfig(t))
	for _, raw := range []string{
		`{"configuration":` + string(configJSON) + `,"` + secret + `":true}`,
		`{"configuration":{"version":1,"` + secret + `":true}}`,
		`{"configuration":` + string(configJSON) + `,"peer":{"password":"` + secret + `"}}`,
	} {
		var options EndpointOptions
		if err := json.Unmarshal([]byte(raw), &options); err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("invalid endpoint JSON accepted or leaked a private field")
		}
	}
}

func TestEndpointOptionsOwnConfigurationAndPeerSelection(t *testing.T) {
	config := testConfig(t)
	_, public := testKeys(t)
	config.Peers = append(config.Peers, Peer{ID: "second", PublicKey: public, Endpoint: "127.0.0.1:10"})
	config.DefaultPeer = "second"
	ep := newTestEndpoint(t, context.Background(), EndpointOptions{Configuration: config})
	if ep.peer != "second" {
		t.Fatal("normalized default peer was ignored")
	}
	explicit := newTestEndpoint(t, context.Background(), EndpointOptions{Configuration: config, Peer: "exit", Resolver: "private-resolver"})
	config.Peers[0].ID = "changed"
	config.Addresses[0] = "10.99.0.2/32"
	if explicit.peer != "exit" || explicit.resolver != "private-resolver" || explicit.config.Peers[0].ID != "exit" || explicit.config.Addresses[0] != "10.7.0.2/32" {
		t.Fatal("endpoint lost explicit selection or retained caller-owned slices")
	}
}

func TestEndpointStartupFailureIsRedactedAndScoped(t *testing.T) {
	registry := transport.NewRegistry()
	var opens atomic.Int32
	if err := registry.RegisterFactory("failed-start", func(context.Context, json.RawMessage) (wgc.Transport, error) {
		opens.Add(1)
		return nil, errors.New("private-transport-password")
	}); err != nil {
		t.Fatal(err)
	}
	config := testConfig(t)
	config.Transport.Type = "failed-start"
	ctx := WithSessionOptions(context.Background(), SessionOptions{Registry: registry})
	ep := newTestEndpoint(t, ctx, EndpointOptions{Configuration: config})
	scope := adapter.NewScope(ctx, nil)
	defer scope.Close()
	if err := ep.Start(adapter.StartStateInitialize, scope); err != nil || opens.Load() != 0 {
		t.Fatal("initialization opened a transport")
	}
	err := ep.Start(adapter.StartStateStart, scope)
	if err == nil || strings.Contains(err.Error(), "password") || opens.Load() != 1 {
		t.Fatalf("startup failure was lost or leaked: %v", err)
	}
	if err := ep.Start(adapter.StartStateStart, scope); err == nil || opens.Load() != 1 {
		t.Fatal("failed endpoint restarted")
	}
}

func TestEndpointStartupTimeoutCancellationAndLateCleanup(t *testing.T) {
	for _, action := range []string{"timeout", "scope close", "endpoint close", "parent cancel"} {
		t.Run(action, func(t *testing.T) {
			registry := transport.NewRegistry()
			entered, release, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			// Deliberately ignore cancellation to check bounded Start and cleanup
			// after an extension eventually returns an owned transport.
			if err := registry.RegisterFactory("late-start", func(context.Context, json.RawMessage) (wgc.Transport, error) {
				close(entered)
				<-release
				return &endpointLateTransport{closed: cleaned}, nil
			}); err != nil {
				t.Fatal(err)
			}
			config := testConfig(t)
			config.Transport.Type = "late-start"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = WithSessionOptions(ctx, SessionOptions{Registry: registry})
			ep := newTestEndpoint(t, ctx, EndpointOptions{Configuration: config})
			if action == "timeout" {
				ep.startTimeout = 50 * time.Millisecond
			}
			scope := adapter.NewScope(ctx, nil)
			defer scope.Close()
			result := make(chan error, 1)
			go func() { result <- ep.Start(adapter.StartStateStart, scope) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("startup did not reach the extension")
			}
			switch action {
			case "scope close":
				_ = scope.Close()
			case "endpoint close":
				_ = ep.Close()
			case "parent cancel":
				cancel()
			}
			select {
			case err := <-result:
				if err == nil || (action == "timeout" && !errors.Is(err, context.DeadlineExceeded)) {
					t.Fatalf("startup cancellation lost: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Start blocked behind a canceled factory")
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-cleaned:
			case <-time.After(time.Second):
				t.Fatal("late transport leaked")
			}
		})
	}
}

type endpointLateTransport struct {
	wgc.Transport
	closed chan struct{}
}

func (t *endpointLateTransport) Close() error { close(t.closed); return nil }
