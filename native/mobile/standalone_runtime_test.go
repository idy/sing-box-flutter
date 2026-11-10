// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/service"
)

func standaloneTunOptions(o option.TunInboundOptions) option.Options {
	return option.Options{Inbounds: []option.Inbound{{Type: C.TypeTun, Options: &o}}}
}

func TestStandaloneEffectiveDNSPlan(t *testing.T) {
	v4 := netip.MustParsePrefix("172.19.0.1/30")
	v6 := netip.MustParsePrefix("fdfe:dcba:9876::1/126")
	for _, tc := range []struct {
		name    string
		opts    option.TunInboundOptions
		mode    string
		servers []string
	}{
		{"default dual", option.TunInboundOptions{AutoRoute: true, Address: []netip.Prefix{v6, v4}}, tun.DNSModeHijack, []string{"172.19.0.2", "fdfe:dcba:9876::2"}},
		{"explicit native", option.TunInboundOptions{AutoRoute: true, DNSMode: tun.DNSModeNative, Address: []netip.Prefix{v6}}, tun.DNSModeNative, []string{"fdfe:dcba:9876::2"}},
		{"native family filter", option.TunInboundOptions{AutoRoute: true, DNSMode: tun.DNSModeNative, Address: []netip.Prefix{v4}, DNSAddress: []netip.Addr{netip.MustParseAddr("2001:db8::53"), netip.MustParseAddr("192.0.2.53")}}, tun.DNSModeNative, []string{"192.0.2.53"}},
		{"disabled explicit", option.TunInboundOptions{AutoRoute: true, DNSMode: tun.DNSModeDisabled, Address: []netip.Prefix{v4}}, tun.DNSModeDisabled, nil},
		{"disabled default", option.TunInboundOptions{Address: []netip.Prefix{v4}}, tun.DNSModeDisabled, nil},
		{"manual native", option.TunInboundOptions{DNSMode: tun.DNSModeNative, Address: []netip.Prefix{v4}}, tun.DNSModeDisabled, nil},
		{"manual hijack", option.TunInboundOptions{DNSMode: tun.DNSModeHijack, Address: []netip.Prefix{v6}}, tun.DNSModeDisabled, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := standaloneDNSPlan(standaloneTunOptions(tc.opts))
			if err != nil {
				t.Fatal(err)
			}
			if !plan.HasTun || plan.DNSMode != tc.mode || !reflect.DeepEqual(iteratorToArray[string](plan.DNSServers()), tc.servers) {
				t.Fatalf("unexpected plan: %+v servers=%v", plan, plan.servers)
			}
		})
	}
	plan, err := standaloneDNSPlan(option.Options{})
	if err != nil || plan.HasTun || plan.DNSMode != tun.DNSModeDisabled {
		t.Fatal("no-TUN low-level plan must stay valid")
	}
	for _, address := range []string{"172.19.0.1/32", "fdfe:dcba:9876::1/128"} {
		if _, err = standaloneDNSPlan(standaloneTunOptions(option.TunInboundOptions{AutoRoute: true, Address: []netip.Prefix{netip.MustParsePrefix(address)}})); err == nil {
			t.Fatal("unusable derived DNS server accepted")
		}
	}
	for _, mode := range []string{tun.DNSModeNative, tun.DNSModeHijack} {
		if _, err = standaloneDNSPlan(standaloneTunOptions(option.TunInboundOptions{DNSMode: mode, Address: []netip.Prefix{netip.MustParsePrefix("172.19.0.1/32")}})); err == nil {
			t.Fatal("manual-route DNS accepted a prefix that panics in the pinned inbound")
		}
	}
	if _, err = standaloneDNSPlan(standaloneTunOptions(option.TunInboundOptions{DNSMode: "invalid-mode", Address: []netip.Prefix{v4}})); err == nil {
		t.Fatal("manual-route invalid DNS mode accepted")
	}
	opts := standaloneTunOptions(option.TunInboundOptions{AutoRoute: true, Address: []netip.Prefix{v4}})
	opts.Inbounds = append(opts.Inbounds, opts.Inbounds[0])
	if _, err = standaloneDNSPlan(opts); err == nil {
		t.Fatal("ambiguous system DNS owners accepted")
	}
}

func TestStandaloneUnsupportedPolicies(t *testing.T) {
	for _, change := range []func(*option.TunInboundOptions){
		func(o *option.TunInboundOptions) { o.IncludeUID = []uint32{501} },
		func(o *option.TunInboundOptions) { o.ExcludeUID = []uint32{501} },
		func(o *option.TunInboundOptions) { o.IncludeUIDRange = []string{"501:502"} },
		func(o *option.TunInboundOptions) { o.ExcludeUIDRange = []string{"501:502"} },
		func(o *option.TunInboundOptions) { o.IncludeAndroidUser = []int{0} },
		func(o *option.TunInboundOptions) { o.IncludePackage = []string{"private.package"} },
		func(o *option.TunInboundOptions) { o.ExcludePackage = []string{"private.package"} },
		func(o *option.TunInboundOptions) { o.StrictRoute = true },
	} {
		o := option.TunInboundOptions{}
		change(&o)
		if err := validateStandaloneOptions(standaloneTunOptions(o)); err == nil || strings.Contains(err.Error(), "private.package") {
			t.Fatal("policy not safely rejected")
		}
	}
}

func TestStandalonePolicyReloadAndOverridesKeepHealthyEngine(t *testing.T) {
	if !slices.Contains(compiledCapabilities(t)["inbounds"], "tun") {
		t.Skip("requires selected TUN")
	}
	engine := &fakeMobileEngine{}
	constructors := 0
	server := &CommandServer{standalone: true, newEngine: func(context.Context, PlatformInterface, option.Options) (mobileEngine, error) {
		constructors++
		return engine, nil
	}}
	defer server.Close()
	if err := server.StartOrReloadService(lifecycleTestConfig, nil); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"include_uid":[501]`, `"exclude_uid":[501]`, `"include_uid_range":["501:502"]`, `"exclude_uid_range":["501:502"]`, `"include_android_user":[0]`, `"include_package":["private.package"]`, `"exclude_package":["private.package"]`, `"strict_route":true`} {
		config := `{"inbounds":[{"type":"tun",` + field + `}],"route":{"final":"fixture"}}`
		if err := server.StartOrReloadService(config, nil); err == nil {
			t.Fatalf("accepted %s", field)
		}
		if engine.closed.Load() != 0 || constructors != 1 {
			t.Fatal("invalid reload disrupted healthy service")
		}
	}
	config := `{"inbounds":[{"type":"tun"}],"route":{"final":"fixture"}}`
	for _, overrides := range []*OverrideOptions{{IncludePackage: newIterator([]string{"private.package"})}, {ExcludePackage: newIterator([]string{"private.package"})}} {
		if err := server.StartOrReloadService(config, overrides); err == nil {
			t.Fatal("post-override package policy accepted")
		}
		if engine.closed.Load() != 0 || constructors != 1 {
			t.Fatal("override failure stopped healthy service")
		}
	}
}

func TestStandalonePrepareConstructsOnceAndKeepsLowLevelNoTun(t *testing.T) {
	var starts atomic.Int32
	var constructors int
	engine := &fakeMobileEngine{start: func() error { starts.Add(1); return nil }}
	server := &CommandServer{standalone: true, newEngine: func(ctx context.Context, platform PlatformInterface, opts option.Options) (mobileEngine, error) {
		constructors++
		engine.ctx = ctx
		return engine, nil
	}}
	defer server.Close()
	plan, err := server.PrepareService(lifecycleTestConfig, nil)
	if err != nil || plan.HasTun {
		t.Fatalf("low-level non-TUN preparation: %v", err)
	}
	if constructors != 1 || starts.Load() != 0 {
		t.Fatal("prepare started or reconstructed engine")
	}
	if _, err = server.PrepareService(lifecycleTestConfig, nil); err == nil {
		t.Fatal("multiple pending preparations accepted")
	}
	if err = server.ActivateService(); err != nil {
		t.Fatal(err)
	}
	if constructors != 1 || starts.Load() != 1 {
		t.Fatal("activation reconstructed engine")
	}
	server.Close()
	if engine.closed.Load() != 1 || engine.ctx.Err() == nil {
		t.Fatal("prepared engine ownership leaked")
	}
}

func TestStandaloneCloseCancelsPreparationAndActivation(t *testing.T) {
	for _, phase := range []string{"prepare", "activate"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan struct{})
			engine := &fakeMobileEngine{}
			server := &CommandServer{standalone: true, newEngine: func(ctx context.Context, _ PlatformInterface, _ option.Options) (mobileEngine, error) {
				engine.ctx = ctx
				block := func() error { close(entered); <-ctx.Done(); return errors.New("private-token") }
				if phase == "prepare" {
					return engine, block()
				}
				engine.start = block
				return engine, nil
			}}
			result := make(chan error, 1)
			go func() {
				_, err := server.PrepareService(lifecycleTestConfig, nil)
				if err == nil {
					err = server.ActivateService()
				}
				result <- err
			}()
			receive(t, entered)
			closed := make(chan struct{})
			go func() { server.Close(); close(closed) }()
			receive(t, closed)
			if err := receive(t, result); err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatal("cancelled operation succeeded or leaked diagnostics")
			}
			if engine.closed.Load() != 1 {
				t.Fatal("partial engine not closed exactly once")
			}
		})
	}
	calls := 0
	server := &CommandServer{standalone: true, newEngine: func(context.Context, PlatformInterface, option.Options) (mobileEngine, error) {
		calls++
		return &fakeMobileEngine{}, nil
	}}
	server.Close()
	if _, err := server.PrepareService(lifecycleTestConfig, nil); err == nil || calls != 0 {
		t.Fatal("pre-entry Close allowed construction")
	}
	if err := server.ActivateService(); err == nil {
		t.Fatal("closed server activated")
	}
}

type standaloneTestMonitorHost struct {
	starts, closes int
	own            []string
}

func (p *standaloneTestMonitorHost) StartDefaultInterfaceMonitor(InterfaceUpdateListener) error {
	p.starts++
	return nil
}
func (p *standaloneTestMonitorHost) CloseDefaultInterfaceMonitor(InterfaceUpdateListener) error {
	p.closes++
	return nil
}
func (p *standaloneTestMonitorHost) RegisterMyInterface(name string) { p.own = append(p.own, name) }

func TestStandaloneMonitorKeepsNativeNetworkingAndIPv6Callbacks(t *testing.T) {
	host := &standaloneTestMonitorHost{}
	server := &CommandServer{monitor: host}
	platform := service.FromContext[adapter.PlatformInterface](server.baseContext())
	if platform == nil || !platform.UsePlatformInterface() || platform.UsePlatformNetworkInterfaces() || platform.UnderNetworkExtension() || platform.UsePlatformAutoDetectInterfaceControl() || !platform.UsePlatformDefaultInterfaceMonitor() {
		t.Fatal("route adapter lost TUN interception or replaced native networking and socket control")
	}
	// Match Box.New: monitor construction precedes platform.Initialize.
	monitor := platform.CreateDefaultInterfaceMonitor(log.NewNOPFactory().Logger()).(*platformDefaultInterfaceMonitor)
	if monitor.platformInterfaceWrapper == nil || monitor.DefaultInterface() != nil {
		t.Fatal("monitor must retain usable empty state before platform initialization")
	}
	_, manager, mobile := newMonitorFixture()
	mobile.interfaces = []*NetworkInterface{{Index: 601, Name: "v6-uplink", Addresses: newIterator([]string{"2001:db8::1/64"})}}
	if err := platform.Initialize(manager); err != nil {
		t.Fatal(err)
	}
	if monitor.networkManager != manager {
		t.Fatal("initialization replaced state already retained by the monitor")
	}
	callbacks := 0
	monitor.RegisterCallback(func(iif *control.Interface, _ int) {
		if iif != nil && iif.Index == 601 {
			callbacks++
		}
	})
	monitor.UpdateDefaultInterface("v6-uplink", 601, false, false)
	if monitor.DefaultInterface() == nil || callbacks != 1 {
		t.Fatal("IPv6-only host path was not published")
	}
	monitor.RegisterMyInterface("utun-fixture")
	monitor.RegisterMyInterface("utun-fixture")
	if !reflect.DeepEqual(host.own, []string{"utun-fixture"}) || !reflect.DeepEqual(monitor.MyInterfaces(), host.own) {
		t.Fatal("own utun registration did not reach host")
	}
	if err := monitor.Close(); err != nil || host.closes != 1 {
		t.Fatal("host monitor not closed")
	}
}
