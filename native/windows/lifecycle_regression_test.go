// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/service"
)

type cleanupTestEndpoint struct {
	adapter.Endpoint
	t         *testing.T
	stages    []adapter.StartStage
	events    []string
	closeOnce sync.Once
	closed    bool
}

func (e *cleanupTestEndpoint) MultiplexEnabled() bool { return true }

func (e *cleanupTestEndpoint) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	e.stages = append(e.stages, stage)
	switch stage {
	case adapter.StartStateInitialize:
		scope.Add(func() error {
			e.closeOnce.Do(func() {
				e.closed = true
				e.events = append(e.events, "device")
			})
			return nil
		})
		scope.Add(func() error {
			e.events = append(e.events, "cancel")
			return nil
		})
	case adapter.StartStateStart:
		scope.Add(func() error {
			if e.closed {
				e.t.Error("constructor device closed before runtime cleanup")
			}
			e.events = append(e.events, "runtime")
			return nil
		})
	}
	return nil
}

type cleanupTestRegistry struct {
	adapter.EndpointRegistry
	endpoint adapter.Endpoint
}

func (r *cleanupTestRegistry) Create(context.Context, adapter.Router, log.ContextLogger, string, string, any) (adapter.Endpoint, error) {
	return r.endpoint, nil
}

func TestConstructorCleanupPreservesIdentityAndRuntimeScopeOrder(t *testing.T) {
	for _, kind := range []string{C.TypeWireGuard, C.TypeOpenConnect, C.TypeOpenVPNClient, C.TypeOpenVPNServer} {
		t.Run(kind, func(t *testing.T) {
			logger := log.NewNOPFactory().Logger()
			constructors := adapter.NewScope(context.Background(), logger)
			defer constructors.Close()
			ep := &cleanupTestEndpoint{t: t}
			registry := &constructorRegistry{EndpointRegistry: &cleanupTestRegistry{endpoint: ep}, cleanup: constructors}
			got, err := registry.Create(context.Background(), nil, logger, "vpn", kind, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != ep {
				t.Fatalf("constructor hid concrete endpoint: %T", got)
			}
			if optional, ok := got.(adapter.OutboundWithMultiplex); !ok || !optional.MultiplexEnabled() {
				t.Fatal("constructor hid an optional endpoint interface")
			}
			runtimeScope := adapter.NewScope(context.Background(), logger)
			defer runtimeScope.Close()
			for _, stage := range []adapter.StartStage{adapter.StartStateInitialize, adapter.StartStateStart} {
				if err := runtimeScope.Start("vpn", got, stage); err != nil {
					t.Fatal(err)
				}
			}
			if !slices.Equal(ep.stages, []adapter.StartStage{adapter.StartStateInitialize, adapter.StartStateInitialize, adapter.StartStateStart}) {
				t.Fatalf("runtime Initialize was suppressed: %v", ep.stages)
			}
			if err := runtimeScope.Close(); err != nil {
				t.Fatal(err)
			}
			if err := constructors.Close(); err != nil {
				t.Fatal(err)
			}
			if err := constructors.Close(); err != nil {
				t.Fatal(err)
			}
			if want := []string{"runtime", "cancel", "device", "cancel"}; !slices.Equal(ep.events, want) {
				t.Fatalf("cleanup order/count = %v, want %v", ep.events, want)
			}
		})
	}
	// Other endpoints' Initialize may start services and must remain untouched.
	ep := &cleanupTestEndpoint{t: t}
	scope := adapter.NewScope(context.Background(), log.NewNOPFactory().Logger())
	defer scope.Close()
	if err := registerConstructorCleanup(scope, "other", C.TypeTailscale, ep); err != nil || len(ep.stages) != 0 {
		t.Fatalf("constructor initialized an endpoint outside the cleanup-only policy: %v", err)
	}
}

func windowsTCPWorkers() (int, string) {
	var trace bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&trace, 2)
	text := trace.String()
	return strings.Count(text, "github.com/sagernet/gvisor/pkg/tcpip/transport/tcp.(*processor).start("), text
}

func requireWindowsTCPWorkers(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, trace := windowsTCPWorkers()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("constructor retained gVisor TCP workers: got %d, want %d\n%s", got, want, trace)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEndpointCleanupAcrossCheckNewFailedConstructionAndStart(t *testing.T) {
	selected := selectedCapabilities(t)
	key := "-----BEGIN OpenVPN Static key V1-----\n" + strings.Repeat("0123456789abcdef", 32) + "\n-----END OpenVPN Static key V1-----"
	keyJSON, _ := json.Marshal(key)
	for _, tc := range []struct {
		kind, concrete, endpoint string
	}{
		{C.TypeWireGuard, "*wireguard.Endpoint", `{"type":"wireguard","tag":"vpn","address":["10.0.0.2/32"],"private_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`},
		{C.TypeOpenConnect, "*openconnect.Endpoint", `{"type":"openconnect","tag":"vpn","server":"https://192.0.2.1"}`},
		{C.TypeOpenVPNClient, "*openvpn.ClientEndpoint", fmt.Sprintf(`{"type":"openvpn-client","tag":"vpn","mode":"static_key","cipher":"AES-256-CBC","server":"192.0.2.1","server_port":1194,"address":["10.8.0.2/24"],"peer_address":"10.8.0.1","static_key":%s}`, keyJSON)},
		{C.TypeOpenVPNServer, "*openvpn.ServerEndpoint", fmt.Sprintf(`{"type":"openvpn-server","tag":"vpn","mode":"static_key","cipher":"AES-256-CBC","network":"tcp","listen":"127.0.0.1","listen_port":1194,"address":["10.8.0.1/24"],"peer_address":"10.8.0.2","static_key":%s}`, keyJSON)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if !slices.Contains(selected["endpoints"], tc.kind) {
				t.Skip("endpoint not selected in this build")
			}
			before, _ := windowsTCPWorkers()
			working := t.TempDir()
			content := `{"endpoints":[` + tc.endpoint + `],"route":{"final":"vpn"}}`
			for range 2 {
				if err := (boxBackend{}).Check(context.Background(), content, working); err != nil {
					t.Fatal(err)
				}
				requireWindowsTCPWorkers(t, before)
			}
			// Both allocations must be freed when box.New fails without a box.
			duplicate := `{"endpoints":[` + tc.endpoint + `,` + tc.endpoint + `],"route":{"final":"vpn"}}`
			if instance, err := createBox(context.Background(), duplicate, working, false); err == nil || instance != nil {
				t.Fatal("duplicate endpoint tags did not fail construction")
			}
			requireWindowsTCPWorkers(t, before)
			if !slices.Contains(selected["inbounds"], "tun") {
				return
			}
			content = `{"inbounds":[{"type":"tun","address":["172.19.0.1/30"],"auto_route":false}],"endpoints":[` + tc.endpoint + `],"route":{"final":"vpn"}}`
			instance, err := (boxBackend{}).New(context.Background(), content, working)
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			engine := instance.(*boxCore)
			ep, found := engine.box.Endpoint().Get("vpn")
			if !found || fmt.Sprintf("%T", ep) != tc.concrete {
				t.Fatalf("native endpoint concrete type lost: %T", ep)
			}
			if tc.kind != C.TypeOpenVPNServer {
				if _, ok := ep.(adapter.InterfaceUpdateListener); !ok {
					t.Fatal("native interface update listener hidden")
				}
				if _, ok := ep.(adapter.OutboundWithPreferredRoutes); !ok {
					t.Fatal("native preferred-route interface hidden")
				}
			}
			// Exercise real runtime Initialize without opening a TUN or network
			// listener, then close both scopes to catch unguarded device closes.
			runtimeScope := adapter.NewScope(engine.ctx, log.NewNOPFactory().Logger())
			defer runtimeScope.Close()
			if err := runtimeScope.Start("vpn", ep, adapter.StartStateInitialize); err != nil {
				t.Fatal(err)
			}
			if err := runtimeScope.Close(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := instance.Close(); err != nil {
					t.Fatal(err)
				}
			}
			requireWindowsTCPWorkers(t, before)
			// A cancelled operation fails box.Start while constructor ownership
			// remains usable for disposal, even before runtime Initialize runs.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			instance, err = (boxBackend{}).New(ctx, content, working)
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			cancel()
			if err := instance.Start(); err == nil {
				t.Fatal("cancelled operation unexpectedly started")
			}
			if err := instance.Close(); err != nil {
				t.Fatal(err)
			}
			requireWindowsTCPWorkers(t, before)
		})
	}
}

func TestEngineContextUsesFreshRegistryAndParentCancellationCause(t *testing.T) {
	type parentKey struct{}
	parent := context.WithValue(context.Background(), parentKey{}, "parent-value")
	parent = service.ContextWith[adapter.EndpointRegistry](parent, &cleanupTestRegistry{})
	parent, stop := context.WithDeadline(parent, time.Now().Add(time.Hour))
	defer stop()
	parent, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	first := engineContext(parent, t.TempDir())
	second := engineContext(parent, t.TempDir())
	firstRegistry := service.RegistryFromContext(first)
	if firstRegistry == service.RegistryFromContext(parent) || firstRegistry == service.RegistryFromContext(second) {
		t.Fatal("operation reused parent or sibling service registry")
	}
	if first.Value(parentKey{}) != "parent-value" {
		t.Fatal("operation lost parent context values")
	}
	if got, ok := first.Deadline(); !ok {
		t.Fatal("operation lost parent deadline")
	} else if want, _ := parent.Deadline(); !got.Equal(want) {
		t.Fatal("operation changed parent deadline")
	}
	if _, seeded := service.FromContext[adapter.EndpointRegistry](first).(*cleanupTestRegistry); seeded {
		t.Fatal("parent endpoint registry replaced the generated selection")
	}
	service.ContextWith[string](first, "first-operation")
	if service.FromContext[string](parent) != "" || service.FromContext[string](second) != "" {
		t.Fatal("operation services escaped into another registry")
	}
	cause := errors.New("operation cancelled")
	cancel(cause)
	if !errors.Is(context.Cause(first), cause) || !errors.Is(first.Err(), context.Canceled) {
		t.Fatal("operation lost parent cancellation cause")
	}
}

func TestNativeRouteOptionsRemainCallerControlled(t *testing.T) {
	if !slices.Contains(selectedCapabilities(t)["outbounds"], "direct") {
		t.Skip("test needs direct")
	}
	for _, tc := range []struct {
		name, route     string
		present, detect bool
	}{
		{"absent", "", false, false},
		{"empty", `,"route":{}`, true, false},
		{"false", `,"route":{"auto_detect_interface":false}`, true, false},
		{"true", `,"route":{"auto_detect_interface":true}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options, err := parseConfig(engineContext(context.Background(), t.TempDir()), `{"outbounds":[{"type":"direct"}]`+tc.route+`}`, false)
			if err != nil {
				t.Fatal(err)
			}
			if (options.Route != nil) != tc.present || options.Route != nil && options.Route.AutoDetectInterface != tc.detect {
				t.Fatal("native route options changed")
			}
		})
	}
}

func TestNativeCacheResolvesCallerPathAndDefault(t *testing.T) {
	if !slices.Contains(selectedCapabilities(t)["outbounds"], "direct") {
		t.Skip("test needs direct")
	}
	for _, name := range []string{"default", "relative", "absolute"} {
		t.Run(name, func(t *testing.T) {
			base := directoryTestCacheRoot(t)
			working := filepath.Join(base, "Host App", "sing_box_flutter")
			lease, err := initializeStorage(working)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			ctx := withStorageContext(context.Background(), lease)
			path := ""
			want := filepath.Join(working, "cache.db")
			if name == "relative" {
				path, want = "profile.db", filepath.Join(working, "profile.db")
			} else if name == "absolute" {
				if runtime.GOOS == "windows" {
					path = filepath.Join(working, "cache", "absolute.db")
				} else {
					external := filepath.Join(base, "external-cache")
					if err := privateDirectory(external); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(external, "absolute.db")
				}
				want = path
			}
			pathJSON, _ := json.Marshal(path)
			content := `{"experimental":{"cache_file":{"enabled":true,"path":` + string(pathJSON) + `}}}`
			for attempt := range 2 {
				engine, err := createBox(ctx, content, working, false)
				if err != nil {
					t.Fatal(err)
				}
				defer engine.Close()
				cache := service.FromContext[adapter.CacheFile](engine.ctx)
				if cache == nil {
					t.Fatal("native cache not registered")
				}
				scope := adapter.NewScope(engine.ctx, log.NewNOPFactory().Logger())
				defer scope.Close()
				if err := scope.Start("cache", cache, adapter.StartStateInitialize); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(want); err != nil {
					t.Fatalf("cache did not use native resolved path %s: %v", want, err)
				}
				if attempt == 0 {
					if err := cache.StoreSelected("test", "selected"); err != nil {
						t.Fatal(err)
					}
				} else if cache.LoadSelected("test") != "selected" {
					t.Fatal("cache did not restore state from the caller's path")
				}
				if err := scope.Close(); err != nil {
					t.Fatal(err)
				}
				if err := engine.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
