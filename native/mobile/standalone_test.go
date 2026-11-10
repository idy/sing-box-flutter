// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"runtime"
	"slices"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"
)

func TestStandaloneConstructorPlatform(t *testing.T) {
	server, err := NewStandaloneCommandServer()
	if runtime.GOOS != "darwin" || C.IsIos {
		if err == nil || server != nil {
			t.Fatal("standalone constructor accepted a non-macOS platform")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if server.platform != nil || server.newEngine == nil {
		t.Fatal("standalone constructor did not select the native engine")
	}
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	server.Pause()
	server.Wake()
	if server.NeedWIFIState() || server.engine != nil || server.cancel != nil {
		t.Fatal("idle standalone lifecycle created a service")
	}
}

type constructorTestHandler struct{ CommandServerHandler }

func TestMobileConstructorStillRequiresHandlerAndPlatform(t *testing.T) {
	handler := &constructorTestHandler{}
	platform := &monitorTestPlatform{}
	for _, tc := range []struct {
		name     string
		handler  CommandServerHandler
		platform PlatformInterface
	}{
		{"both absent", nil, nil},
		{"handler absent", nil, platform},
		{"platform absent", handler, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if server, err := NewCommandServer(tc.handler, tc.platform); err == nil || server != nil {
				t.Fatal("mobile constructor accepted a missing mandatory argument")
			}
		})
	}
	server, err := NewCommandServer(handler, platform)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if server.platform != platform || server.newEngine == nil {
		t.Fatal("mobile constructor lost its host platform")
	}
}

func TestBoxEngineNativeAndMobilePlatformSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform PlatformInterface
	}{
		{"native", nil},
		{"mobile", &monitorTestPlatform{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := cacheTestContext(t.TempDir())
			options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(`{"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}`))
			if err != nil {
				t.Fatal(err)
			}
			mobile, err := newBoxEngine(ctx, tc.platform, options)
			if err != nil {
				t.Fatal(err)
			}
			engine := mobile.(*boxEngine)
			// Construct and close only. Start would open native monitors or a TUN.
			t.Cleanup(func() {
				if err := engine.Close(); err != nil {
					t.Error(err)
				}
			})
			platform := service.FromContext[adapter.PlatformInterface](engine.ctx)
			if tc.platform == nil {
				if platform != nil {
					t.Fatal("native engine installed a platform override")
				}
			} else if wrapper, ok := platform.(*platformInterfaceWrapper); !ok || wrapper.iif != tc.platform {
				t.Fatal("mobile engine did not preserve its platform wrapper")
			}
			if engine.pause == nil || service.FromContext[pause.Manager](engine.ctx) == nil {
				t.Fatal("engine lost its pause lifecycle")
			}
		})
	}
}

func TestStandaloneServiceLifecycleUsesNativeContext(t *testing.T) {
	if runtime.GOOS != "darwin" || C.IsIos {
		t.Skip("standalone server requires macOS")
	}
	server, err := NewStandaloneCommandServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var engines []*fakeMobileEngine
	server.newEngine = func(ctx context.Context, platform PlatformInterface, _ option.Options) (mobileEngine, error) {
		wrapper, ok := service.FromContext[adapter.PlatformInterface](ctx).(*standalonePlatformWrapper)
		if platform != nil || !ok || !wrapper.UsePlatformInterface() || wrapper.UsePlatformDefaultInterfaceMonitor() || wrapper.UsePlatformNetworkInterfaces() || wrapper.UsePlatformAutoDetectInterfaceControl() {
			t.Fatal("standalone lifecycle lost its native add-only route boundary")
		}
		engine := &fakeMobileEngine{ctx: ctx}
		engines = append(engines, engine)
		return engine, nil
	}
	if err = server.StartOrReloadService(lifecycleTestConfig, nil); err != nil {
		t.Fatal(err)
	}
	first := engines[0]
	server.Pause()
	server.Wake()
	if first.pauses.Load() != 1 || first.wakes.Load() != 1 || !server.NeedWIFIState() {
		t.Fatal("standalone server did not forward lifecycle operations")
	}
	if err = server.StartOrReloadService(`{invalid`, nil); err == nil || first.closed.Load() != 0 {
		t.Fatal("invalid standalone reload destroyed the active service")
	}
	if err = server.StartOrReloadService(lifecycleTestConfig, nil); err != nil {
		t.Fatal(err)
	}
	if first.closed.Load() != 1 || first.ctx.Err() == nil {
		t.Fatal("standalone reload did not cancel and close the old service")
	}
	if err = server.CloseService(); err != nil {
		t.Fatal(err)
	}
	if server.NeedWIFIState() || engines[1].closed.Load() != 1 || engines[1].ctx.Err() == nil {
		t.Fatal("standalone stop retained the service")
	}
	if err = server.StartOrReloadService(lifecycleTestConfig, nil); err != nil {
		t.Fatal(err)
	}
	server.Close()
	server.Close()
	if len(engines) != 3 {
		t.Fatalf("standalone restart created %d engines, want 3", len(engines))
	}
	for _, engine := range engines {
		if engine.closed.Load() != 1 || engine.ctx.Err() == nil {
			t.Fatal("standalone lifecycle leaked or double-closed an engine")
		}
	}
	if server.Start() == nil || server.StartOrReloadService(lifecycleTestConfig, nil) == nil || server.CloseService() == nil {
		t.Fatal("closed standalone server accepted work")
	}
	server.Pause()
	server.Wake()
	if server.NeedWIFIState() {
		t.Fatal("closed standalone server retained an engine")
	}
}

func TestStandaloneTunConfigPreflight(t *testing.T) {
	ctx := mobileBaseContext(nil)
	if service.FromContext[adapter.PlatformInterface](ctx) != nil {
		t.Fatal("nil-platform base context installed a platform override")
	}
	selected := compiledCapabilities(t)
	if !slices.Contains(selected["inbounds"], "tun") || !slices.Contains(selected["outbounds"], "direct") {
		t.Skip("requires tun and direct selected")
	}
	content := `{"inbounds":[{"type":"tun","tag":"tun","address":["172.19.0.1/30"],"auto_route":true}],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct","auto_detect_interface":true}}`
	if slices.Contains(selected["dns"], "local") {
		content = `{"dns":{"servers":[{"type":"local","tag":"local"}]},` + content[1:]
	}
	if _, err := parseConfig(ctx, content); err != nil {
		t.Fatal(err)
	}
	if hasTun, err := HasTunInbound(content); err != nil || !hasTun {
		t.Fatalf("native TUN preflight failed: hasTun=%v err=%v", hasTun, err)
	}
	// Validation constructs and closes the box without starting utun, changing
	// routes or DNS, or requiring elevated privileges.
	if err := CheckConfig(content); err != nil {
		t.Fatal(err)
	}
}
