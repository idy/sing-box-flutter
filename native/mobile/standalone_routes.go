// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"errors"
	"net/netip"
	"slices"
	"sync"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common"
)

type standaloneRoute struct {
	destination    netip.Prefix
	interfaceRoute bool // Directly attached, rather than a covering gateway route.
	interfaceIndex int
	scoped         bool
}

type standaloneRoutePlan struct {
	destinations []netip.Prefix
	addresses    []netip.Prefix
}

// BuildAutoRouteRanges appends interface subnets to its input slices on Darwin.
// Keep caller options immutable and deduplicate those same-interface routes.
func standaloneRouteRanges(options tun.Options) ([]netip.Prefix, error) {
	options.Inet4RouteAddress = slices.Clone(options.Inet4RouteAddress)
	options.Inet6RouteAddress = slices.Clone(options.Inet6RouteAddress)
	ranges, err := options.BuildAutoRouteRanges(false)
	if err != nil {
		return nil, errors.New("invalid standalone route addresses")
	}
	var result []netip.Prefix
	seen := make(map[netip.Prefix]bool)
	for _, prefix := range ranges {
		prefix = prefix.Masked()
		if !seen[prefix] {
			seen[prefix] = true
			result = append(result, prefix)
		}
	}
	return result, nil
}

// Use the same effective ranges as native Darwin sing-tun. Inspection is an
// early diagnostic only; add-only installation is the authoritative boundary.
func buildStandaloneRoutePlan(options option.Options) (standaloneRoutePlan, error) {
	var plan standaloneRoutePlan
	seen := make(map[netip.Prefix]bool)
	for _, inbound := range options.Inbounds {
		if inbound.Type != C.TypeTun {
			continue
		}
		o, ok := inbound.Options.(*option.TunInboundOptions)
		if !ok || o == nil {
			return plan, errors.New("invalid standalone TUN options")
		}
		// The platform-open path does not load dynamic route sets. Require static
		// ranges rather than silently starting with an incomplete routing policy.
		if len(o.RouteAddressSet) > 0 || len(o.RouteExcludeAddressSet) > 0 {
			return plan, errors.New("standalone route sets require explicit static route addresses")
		}
		native := tun.Options{
			AutoRoute:                o.AutoRoute,
			Inet4Address:             common.Filter(o.Address, func(p netip.Prefix) bool { return p.Addr().Is4() }),
			Inet6Address:             common.Filter(o.Address, func(p netip.Prefix) bool { return p.Addr().Is6() }),
			Inet4RouteAddress:        common.Filter(o.RouteAddress, func(p netip.Prefix) bool { return p.Addr().Is4() }),
			Inet6RouteAddress:        common.Filter(o.RouteAddress, func(p netip.Prefix) bool { return p.Addr().Is6() }),
			Inet4RouteExcludeAddress: common.Filter(o.RouteExcludeAddress, func(p netip.Prefix) bool { return p.Addr().Is4() }),
			Inet6RouteExcludeAddress: common.Filter(o.RouteExcludeAddress, func(p netip.Prefix) bool { return p.Addr().Is6() }),
		}
		ranges, err := standaloneRouteRanges(native)
		if err != nil {
			return plan, err
		}
		for _, prefix := range ranges {
			if seen[prefix] {
				return plan, errors.New("standalone TUN routes overlap between interfaces")
			}
			seen[prefix] = true
			plan.destinations = append(plan.destinations, prefix)
		}
		for _, prefix := range o.Address {
			plan.addresses = append(plan.addresses, prefix.Masked())
		}
	}
	return plan, nil
}

func (s *CommandServer) checkStandaloneRoutes(plan standaloneRoutePlan) error {
	if len(plan.destinations) == 0 && len(plan.addresses) == 0 {
		return nil
	}
	read := s.routeTable
	if read == nil {
		read = readStandaloneRoutes
	}
	existing, err := read()
	if err != nil {
		return errors.New("could not inspect standalone routes")
	}
	for _, installed := range existing {
		if !installed.destination.IsValid() {
			return errors.New("could not inspect standalone routes")
		}
		prefix := installed.destination.Masked()
		for _, destination := range plan.destinations {
			if prefix == destination {
				return errors.New("standalone routes conflict with existing network routes")
			}
		}
		// Covering gateway routes (including split defaults) do not establish
		// ownership of an interface subnet. Directly attached routes do.
		if !installed.interfaceRoute || prefix.Bits() == 0 {
			continue
		}
		for _, address := range plan.addresses {
			if prefix.Overlaps(address) {
				return errors.New("standalone addresses conflict with existing network routes")
			}
		}
	}
	return nil
}

// Keep native utun creation, addresses, packet I/O and monitor registration, but
// suppress every inner route. In particular, NativeTun.Close must never run its
// unconditional route deletion loop, even after a partially successful Start.
func newStandaloneTun(options tun.Options, create func(tun.Options) (tun.Tun, error), add func(string, netip.Prefix, netip.Addr) error) (tun.Tun, error) {
	if options.FileDescriptor != 0 || options.EXP_ExternalConfiguration || options.InterfaceScope {
		return nil, errors.New("unsupported standalone TUN ownership")
	}
	routes, err := standaloneRouteRanges(options)
	if err != nil {
		return nil, err
	}
	inner := options
	inner.Inet4RouteAddress, inner.Inet6RouteAddress = nil, nil
	inner.Inet4RouteExcludeAddress = []netip.Prefix{netip.PrefixFrom(netip.IPv4Unspecified(), 0)}
	inner.Inet6RouteExcludeAddress = []netip.Prefix{netip.PrefixFrom(netip.IPv6Unspecified(), 0)}
	native, err := create(inner)
	if err != nil {
		if native != nil {
			_ = native.Close()
		}
		return nil, errors.New("could not open standalone TUN")
	}
	darwin, ok := native.(tun.DarwinTUN)
	if !ok {
		if native != nil {
			_ = native.Close()
		}
		return nil, errors.New("could not open standalone TUN")
	}
	return &standaloneTun{DarwinTUN: darwin, routes: routes,
		gateway4: options.Inet4GatewayAddr(), gateway6: options.Inet6GatewayAddr(), addRoute: add}, nil
}

type standaloneTun struct {
	tun.DarwinTUN
	access             sync.Mutex
	routes             []netip.Prefix
	gateway4, gateway6 netip.Addr
	addRoute           func(string, netip.Prefix, netip.Addr) error
	started, closed    bool
	closeErr           error
}

var _ tun.DarwinTUN = (*standaloneTun)(nil)

func (t *standaloneTun) Start() error {
	t.access.Lock()
	defer t.access.Unlock()
	if t.closed {
		return errors.New("standalone TUN is closed")
	}
	if t.started {
		return nil
	}
	if err := t.DarwinTUN.Start(); err != nil {
		_ = t.closeLocked()
		return errors.New("could not start standalone TUN")
	}
	name, err := t.Name()
	if err != nil {
		_ = t.closeLocked()
		return errors.New("could not identify standalone TUN")
	}
	for _, destination := range t.routes {
		gateway := t.gateway6
		if destination.Addr().Is4() {
			gateway = t.gateway4
		}
		if err = t.addRoute(name, destination, gateway); err != nil {
			// Closing the owned utun removes routes attached to that interface,
			// including earlier successful adds. Never delete routes by prefix.
			_ = t.closeLocked()
			return errors.New("could not add standalone route")
		}
	}
	t.started = true
	return nil
}

func (t *standaloneTun) Close() error {
	t.access.Lock()
	defer t.access.Unlock()
	return t.closeLocked()
}

func (t *standaloneTun) closeLocked() error {
	if !t.closed {
		t.closed = true
		if err := t.DarwinTUN.Close(); err != nil {
			t.closeErr = errors.New("could not close standalone TUN")
		}
	}
	return t.closeErr
}

func (t *standaloneTun) UpdateRouteOptions(tun.Options) error {
	// Never forward new options to NativeTun: that would restore its unsafe
	// replacement/deletion paths. Standalone policy changes require a restart.
	return errors.New("standalone route updates require a restart")
}
