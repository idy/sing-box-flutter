// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/service"
)

func prefixes(values ...string) []netip.Prefix {
	var result []netip.Prefix
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}

func routeEntries(direct bool, values ...string) []standaloneRoute {
	var result []standaloneRoute
	for _, prefix := range prefixes(values...) {
		result = append(result, standaloneRoute{destination: prefix, interfaceRoute: direct})
	}
	return result
}

func TestStandaloneRouteConflicts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		plan     standaloneRoutePlan
		existing []standaloneRoute
		fails    bool
	}{
		{"v4 host owned by another VPN", standaloneRoutePlan{destinations: prefixes("198.18.20.1/32")}, routeEntries(false, "198.18.20.1/32"), true},
		{"v6 host owned by another VPN", standaloneRoutePlan{destinations: prefixes("2001:db8::1/128")}, routeEntries(false, "2001:db8::1/128"), true},
		{"normalized network", standaloneRoutePlan{destinations: prefixes("198.18.20.0/24")}, routeEntries(false, "198.18.20.99/24"), true},
		{"split default collision", standaloneRoutePlan{destinations: prefixes("128.0.0.0/1")}, routeEntries(false, "128.0.0.0/1"), true},
		{"explicit default collision", standaloneRoutePlan{destinations: prefixes("0.0.0.0/0")}, routeEntries(false, "0.0.0.0/0"), true},
		{"physical defaults are expected", standaloneRoutePlan{destinations: prefixes("198.18.20.1/32", "2001:db8::1/128"), addresses: prefixes("198.18.10.0/30", "fd00::/126")}, routeEntries(false, "0.0.0.0/0", "::/0", "192.168.1.0/24", "fe80::/64"), false},
		{"unrelated more specific routes", standaloneRoutePlan{destinations: prefixes("128.0.0.0/1")}, routeEntries(false, "192.168.1.0/24"), false},
		{"interface subnet overlaps existing host", standaloneRoutePlan{addresses: prefixes("198.18.10.0/30")}, routeEntries(true, "198.18.10.1/32"), true},
		{"interface subnet overlaps existing network", standaloneRoutePlan{addresses: prefixes("198.18.10.0/30")}, routeEntries(true, "198.18.0.0/16"), true},
		{"IPv6 interface subnet conflict", standaloneRoutePlan{addresses: prefixes("fd00::/126")}, routeEntries(true, "fd00::/64"), true},
		{"split defaults do not own interface subnets", standaloneRoutePlan{destinations: prefixes("203.0.113.0/24"), addresses: prefixes("172.19.0.0/30", "fd00::/126")}, routeEntries(false, "0.0.0.0/1", "128.0.0.0/1", "::/1", "8000::/1"), false},
		{"covering gateway does not own interface subnet", standaloneRoutePlan{addresses: prefixes("198.18.10.0/30")}, routeEntries(false, "198.18.0.0/16"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &CommandServer{routeTable: func() ([]standaloneRoute, error) { return tc.existing, nil }}
			err := server.checkStandaloneRoutes(tc.plan)
			if (err != nil) != tc.fails {
				t.Fatalf("conflict error=%v, want failure=%v", err, tc.fails)
			}
			if err != nil && (strings.Contains(err.Error(), "198.18.") || strings.Contains(err.Error(), "fd00:")) {
				t.Fatal("route diagnostics exposed private network addresses")
			}
		})
	}
}

func TestStandaloneRouteInspectionFailsClosed(t *testing.T) {
	calls := 0
	server := &CommandServer{routeTable: func() ([]standaloneRoute, error) {
		calls++
		return nil, errors.New("private-network-details")
	}}
	if err := server.checkStandaloneRoutes(standaloneRoutePlan{}); err != nil || calls != 0 {
		t.Fatal("no-TUN service inspected host routing")
	}
	if err := server.checkStandaloneRoutes(standaloneRoutePlan{destinations: prefixes("198.18.20.1/32")}); err == nil || strings.Contains(err.Error(), "private-network") {
		t.Fatal("route inspection failure was ignored or leaked details")
	}
}

func TestStandaloneEffectiveRoutePlanMatchesNative(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin native route range contract")
	}
	for _, auto := range []bool{false, true} {
		options := option.TunInboundOptions{
			AutoRoute:           auto,
			Address:             prefixes("198.18.10.1/30", "fd00::1/126"),
			RouteAddress:        prefixes("198.18.20.0/24", "2001:db8::/64"),
			RouteExcludeAddress: prefixes("198.18.20.128/25", "2001:db8::/65"),
		}
		plan, err := buildStandaloneRoutePlan(standaloneTunOptions(options))
		if err != nil {
			t.Fatal(err)
		}
		native := tun.Options{AutoRoute: auto, Inet4Address: slices.Clone(options.Address[:1]), Inet6Address: slices.Clone(options.Address[1:]),
			Inet4RouteAddress: slices.Clone(options.RouteAddress[:1]), Inet6RouteAddress: slices.Clone(options.RouteAddress[1:]),
			Inet4RouteExcludeAddress: slices.Clone(options.RouteExcludeAddress[:1]), Inet6RouteExcludeAddress: slices.Clone(options.RouteExcludeAddress[1:])}
		want, err := native.BuildAutoRouteRanges(false)
		if err != nil || !slices.Equal(plan.destinations, want) {
			t.Fatalf("guard and native route ranges differ: guard=%v native=%v err=%v", plan.destinations, want, err)
		}
		if !slices.Equal(plan.addresses, prefixes("198.18.10.0/30", "fd00::/126")) {
			t.Fatal("route guard lost assigned subnets")
		}
	}
	for _, field := range []string{"route_address_set", "route_exclude_address_set"} {
		opts := option.TunInboundOptions{Address: prefixes("198.18.10.1/30")}
		if field == "route_address_set" {
			opts.RouteAddressSet = []string{"private-set"}
		} else {
			opts.RouteExcludeAddressSet = []string{"private-set"}
		}
		if _, err := buildStandaloneRoutePlan(standaloneTunOptions(opts)); err == nil || strings.Contains(err.Error(), "private-set") {
			t.Fatal("accepted unresolved route set or exposed its identifier")
		}
	}
}

func TestStandaloneChecksRoutesAtActivationBeforeStartingEngine(t *testing.T) {
	selected := compiledCapabilities(t)
	if !slices.Contains(selected["inbounds"], "tun") || !slices.Contains(selected["outbounds"], "direct") {
		t.Skip("requires selected TUN and direct")
	}
	config := `{"inbounds":[{"type":"tun","address":["198.18.10.1/30"],"auto_route":true,"route_address":["198.18.20.1/32"],"dns_mode":"disabled"}],"outbounds":[{"type":"direct","tag":"direct"}]}`
	for _, prepared := range []bool{false, true} {
		for _, inspectionError := range []bool{false, true} {
			engine := &fakeMobileEngine{}
			started, inspected := false, 0
			engine.start = func() error { started = true; return nil }
			server := &CommandServer{standalone: true,
				newEngine: func(context.Context, PlatformInterface, option.Options) (mobileEngine, error) { return engine, nil },
				routeTable: func() ([]standaloneRoute, error) {
					inspected++
					if inspectionError {
						return nil, errors.New("private-route-details")
					}
					return routeEntries(false, "198.18.20.1/32"), nil
				},
			}
			if prepared {
				if _, err := server.PrepareService(config, nil); err != nil {
					t.Fatal(err)
				}
				if inspected != 0 {
					t.Fatal("preparation inspected host routes")
				}
			}
			var err error
			if prepared {
				err = server.ActivateService()
			} else {
				err = server.StartOrReloadService(config, nil)
			}
			if err == nil || strings.Contains(err.Error(), "private-route") || started || inspected != 1 || engine.closed.Load() != 1 {
				t.Fatalf("conflict reached engine Start or leaked cleanup: err=%v started=%v inspected=%d closed=%d", err, started, inspected, engine.closed.Load())
			}
			if err := server.CloseWithError(); err != nil || engine.closed.Load() != 1 {
				t.Fatal("failed activation leaked or double-closed its engine")
			}
		}
	}
}

func TestStandaloneRoutePlanDeduplicatesWithinOneInterface(t *testing.T) {
	options := standaloneTunOptions(option.TunInboundOptions{Address: prefixes("198.18.10.1/30"), RouteAddress: prefixes("198.18.10.0/30", "198.18.10.0/30")})
	plan, err := buildStandaloneRoutePlan(options)
	if err != nil || !slices.Equal(plan.destinations, prefixes("198.18.10.0/30")) {
		t.Fatalf("same-interface duplicates rejected: %+v, %v", plan, err)
	}
	options.Inbounds = append(options.Inbounds, options.Inbounds[0])
	if _, err = buildStandaloneRoutePlan(options); err == nil {
		t.Fatal("duplicate routes across interfaces accepted")
	}
}

type standaloneRouteTestTun struct {
	tun.DarwinTUN
	options                 tun.Options
	starts, closes, updates int
	startErr, closeErr      error
	onClose                 func()
}

func (t *standaloneRouteTestTun) Name() (string, error) { return "utun-fixture", nil }
func (t *standaloneRouteTestTun) Start() error {
	t.starts++
	ranges, err := t.options.BuildAutoRouteRanges(false)
	if err != nil || len(ranges) != 0 {
		return errors.New("inner native routing was not suppressed")
	}
	return t.startErr
}
func (t *standaloneRouteTestTun) Close() error {
	t.closes++
	if t.onClose != nil {
		t.onClose()
	}
	return t.closeErr
}
func (t *standaloneRouteTestTun) UpdateRouteOptions(tun.Options) error {
	t.updates++
	return nil
}

func standaloneRouteTestOptions() tun.Options {
	return tun.Options{Name: "utun-fixture", MTU: 9000, AutoRoute: true,
		Inet4Address: prefixes("198.18.10.1/30"), Inet6Address: prefixes("fd00::1/126"),
		Inet4RouteAddress: prefixes("198.18.20.1/32", "198.18.20.2/32"),
		Inet6RouteAddress: prefixes("2001:db8::/64"), DNSMode: tun.DNSModeHijack}
}

func TestStandaloneNativeRouteSuppressionAndUpdateRejection(t *testing.T) {
	options := standaloneRouteTestOptions()
	original4, original6 := slices.Clone(options.Inet4RouteAddress), slices.Clone(options.Inet6RouteAddress)
	native := &standaloneRouteTestTun{}
	var installed []netip.Prefix
	wrapped, err := newStandaloneTun(options, func(inner tun.Options) (tun.Tun, error) {
		native.options = inner
		if inner.AutoRoute != options.AutoRoute || inner.DNSMode != options.DNSMode || inner.MTU != options.MTU || !slices.Equal(inner.Inet4Address, options.Inet4Address) || !slices.Equal(inner.Inet6Address, options.Inet6Address) {
			t.Fatal("native address, DNS or packet options changed")
		}
		return native, nil
	}, func(name string, prefix netip.Prefix, gateway netip.Addr) error {
		if name != "utun-fixture" || !gateway.IsValid() || gateway.Is4() != prefix.Addr().Is4() {
			t.Fatal("route lost its native interface or gateway family")
		}
		installed = append(installed, prefix)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrapped.Close() })
	if _, ok := wrapped.(tun.DarwinTUN); !ok {
		t.Fatal("wrapper lost Darwin batch packet I/O")
	}
	if err = wrapped.Start(); err != nil {
		t.Fatal(err)
	}
	want, err := standaloneRouteRanges(options)
	if err != nil || !slices.Equal(installed, want) || !slices.Equal(options.Inet4RouteAddress, original4) || !slices.Equal(options.Inet6RouteAddress, original6) {
		t.Fatalf("effective route plan or caller options changed: %v, %v", installed, err)
	}
	if err = wrapped.Start(); err != nil || native.starts != 1 || len(installed) != len(want) {
		t.Fatal("repeated Start reinstalled routes")
	}
	if err = wrapped.UpdateRouteOptions(options); err == nil || native.updates != 0 {
		t.Fatal("route update restored native replacement/deletion")
	}
	if err = wrapped.Close(); err != nil {
		t.Fatal(err)
	}
	if err = wrapped.Close(); err != nil || native.closes != 1 || wrapped.Start() == nil {
		t.Fatal("native interface was restarted or closed more than once")
	}
}

func TestStandaloneRouteAppearingAfterPreflightAndPartialStart(t *testing.T) {
	for _, failAt := range []int{0, 1} {
		t.Run([]string{"first-add", "partial-add"}[failAt], func(t *testing.T) {
			options := standaloneRouteTestOptions()
			server := &CommandServer{routeTable: func() ([]standaloneRoute, error) { return nil, nil }}
			if err := server.checkStandaloneRoutes(standaloneRoutePlan{destinations: options.Inet4RouteAddress}); err != nil {
				t.Fatal(err)
			}
			// A foreign route arrives after preflight. Interface teardown removes
			// only our own routes; it must never remove the foreign destination.
			foreign := options.Inet4RouteAddress[failAt]
			table := map[netip.Prefix]string{foreign: "other-vpn"}
			native := &standaloneRouteTestTun{onClose: func() {
				for prefix, owner := range table {
					if owner == "our-utun" {
						delete(table, prefix)
					}
				}
			}}
			adds := 0
			wrapped, err := newStandaloneTun(options, func(inner tun.Options) (tun.Tun, error) {
				native.options = inner
				return native, nil
			}, func(_ string, prefix netip.Prefix, _ netip.Addr) error {
				adds++
				if _, exists := table[prefix]; exists {
					return syscall.EEXIST
				}
				table[prefix] = "our-utun"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = wrapped.Start(); err == nil || err.Error() != "could not add standalone route" {
				t.Fatalf("late conflict did not fail safely: %v", err)
			}
			if adds != failAt+1 || native.closes != 1 || !reflect.DeepEqual(table, map[netip.Prefix]string{foreign: "other-vpn"}) {
				t.Fatalf("partial startup leaked or replaced routes: adds=%d closes=%d table=%v", adds, native.closes, table)
			}
			if err = wrapped.Close(); err != nil || native.closes != 1 {
				t.Fatal("engine cleanup double-closed the native interface")
			}
		})
	}
}

func TestStandaloneNativeRouteStartAndCleanupErrors(t *testing.T) {
	for _, failCreate := range []bool{false, true} {
		native := &standaloneRouteTestTun{startErr: errors.New("private-start-details"), closeErr: errors.New("private-close-details")}
		wrapped, err := newStandaloneTun(standaloneRouteTestOptions(), func(inner tun.Options) (tun.Tun, error) {
			native.options = inner
			if failCreate {
				return native, errors.New("private-create-details")
			}
			return native, nil
		}, func(string, netip.Prefix, netip.Addr) error {
			t.Fatal("native failure still installed a route")
			return nil
		})
		if failCreate {
			if err == nil || strings.Contains(err.Error(), "private") || native.closes != 1 || wrapped != nil {
				t.Fatalf("failed creation retained its native interface: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = wrapped.Start(); err == nil || strings.Contains(err.Error(), "private") || native.closes != 1 {
			t.Fatalf("failed native start was not closed: %v", err)
		}
		for range 2 {
			if err = wrapped.Close(); err == nil || err.Error() != "could not close standalone TUN" || native.closes != 1 {
				t.Fatalf("native cleanup error was lost, leaked or retried: %v", err)
			}
		}
	}
}

func TestStandaloneRouteBoundaryInstalledWithoutMonitor(t *testing.T) {
	for _, monitored := range []bool{false, true} {
		server := &CommandServer{standalone: true}
		if monitored {
			server.monitor = &standaloneTestMonitorHost{}
		}
		platform, ok := service.FromContext[adapter.PlatformInterface](server.baseContext()).(*standalonePlatformWrapper)
		if !ok || !platform.UsePlatformInterface() || platform.UsePlatformDefaultInterfaceMonitor() != monitored || platform.UsePlatformNetworkInterfaces() || platform.UsePlatformAutoDetectInterfaceControl() || platform.UnderNetworkExtension() {
			t.Fatal("standalone route interception changed native monitoring, enumeration or socket binding")
		}
		opened := false
		platform.openTun = func(options tun.Options) (tun.Tun, error) { opened = true; return nil, errors.New("test sentinel") }
		if _, err := platform.OpenInterface(&tun.Options{}, option.TunPlatformOptions{}); err == nil || !opened {
			t.Fatal("standalone TUN bypassed the route boundary")
		}
	}
}

func TestStandaloneRouteCancellationClosesPartialInterface(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		entered := make(chan struct{})
		native := &standaloneRouteTestTun{}
		var wrapped tun.Tun
		engine := &fakeMobileEngine{}
		server := &CommandServer{standalone: true,
			newEngine: func(ctx context.Context, _ PlatformInterface, _ option.Options) (mobileEngine, error) {
				platform := service.FromContext[adapter.PlatformInterface](ctx).(*standalonePlatformWrapper)
				platform.openTun = func(options tun.Options) (tun.Tun, error) {
					return newStandaloneTun(options, func(inner tun.Options) (tun.Tun, error) {
						native.options = inner
						return native, nil
					}, func(string, netip.Prefix, netip.Addr) error {
						close(entered)
						<-ctx.Done()
						return ctx.Err()
					})
				}
				engine.start = func() error {
					var err error
					options := standaloneRouteTestOptions()
					wrapped, err = platform.OpenInterface(&options, option.TunPlatformOptions{})
					if err != nil {
						return err
					}
					return wrapped.Start()
				}
				engine.close = func() error {
					if wrapped != nil {
						return wrapped.Close()
					}
					return nil
				}
				return engine, nil
			},
		}
		if prepared {
			if _, err := server.PrepareService(lifecycleTestConfig, nil); err != nil {
				t.Fatal(err)
			}
		}
		result := make(chan error, 1)
		go func() {
			if prepared {
				result <- server.ActivateService()
			} else {
				result <- server.StartOrReloadService(lifecycleTestConfig, nil)
			}
		}()
		receive(t, entered)
		closed := make(chan error, 1)
		go func() { closed <- server.CloseWithError() }()
		if err := receive(t, result); err == nil || strings.Contains(err.Error(), "198.18.") {
			t.Fatalf("cancelled interface start succeeded or leaked routes: %v", err)
		}
		if err := receive(t, closed); err != nil || native.closes != 1 || engine.closed.Load() != 1 {
			t.Fatalf("cancelled native interface was leaked/double-closed: %v", err)
		}
	}
}

func TestStandaloneRouteCleanupFailureSurvivesDrainedUpstreamScope(t *testing.T) {
	native := &standaloneRouteTestTun{closeErr: errors.New("private-close-details")}
	engine := &fakeMobileEngine{}
	server := &CommandServer{standalone: true,
		newEngine: func(ctx context.Context, _ PlatformInterface, _ option.Options) (mobileEngine, error) {
			platform := service.FromContext[adapter.PlatformInterface](ctx).(*standalonePlatformWrapper)
			platform.openTun = func(options tun.Options) (tun.Tun, error) {
				return newStandaloneTun(options, func(inner tun.Options) (tun.Tun, error) {
					native.options = inner
					return native, nil
				}, func(string, netip.Prefix, netip.Addr) error { return syscall.EEXIST })
			}
			scope := adapter.NewScope(ctx, log.NewNOPFactory().Logger())
			engine.start = func() error {
				options := standaloneRouteTestOptions()
				iface, err := platform.OpenInterface(&options, option.TunPlatformOptions{})
				if err != nil {
					return err
				}
				scope.Add(iface.Close)
				err = iface.Start()
				// Match Box.Start: discard a failed cleanup and drain the scope.
				_ = scope.Close()
				return err
			}
			engine.close = scope.Close
			return engine, nil
		},
	}
	if err := server.StartOrReloadService(lifecycleTestConfig, nil); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("route failure succeeded or leaked details: %v", err)
	}
	if !server.closed || server.closeErr == nil || native.closes != 1 || engine.closed.Load() != 1 {
		t.Fatal("upstream cleanup drain hid a failed native close")
	}
	if err := server.StartOrReloadService(lifecycleTestConfig, nil); err == nil {
		t.Fatal("failed native cleanup allowed another start")
	}
	for range 2 {
		if err := server.CloseWithError(); err == nil || strings.Contains(err.Error(), "private") || native.closes != 1 {
			t.Fatalf("cleanup failure was lost, leaked or retried: %v", err)
		}
	}
}
