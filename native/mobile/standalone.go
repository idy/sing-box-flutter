// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"errors"
	"net/netip"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/service"
)

// StandaloneDNSPlan contains only the effective system resolver configuration;
// upstream DNS transports and credential-bearing config never cross this API.
type StandaloneDNSPlan struct {
	HasTun  bool
	DNSMode string
	servers []string
}

func (p *StandaloneDNSPlan) DNSServers() StringIterator { return newIterator(p.servers) }

func validateStandaloneOptions(options option.Options) error {
	for _, inbound := range options.Inbounds {
		if inbound.Type != C.TypeTun {
			continue
		}
		o, ok := inbound.Options.(*option.TunInboundOptions)
		if !ok || o == nil {
			return errors.New("invalid standalone TUN options")
		}
		if o.StrictRoute || len(o.IncludeUID) > 0 || len(o.ExcludeUID) > 0 ||
			len(o.IncludeUIDRange) > 0 || len(o.ExcludeUIDRange) > 0 ||
			len(o.IncludeAndroidUser) > 0 || len(o.IncludePackage) > 0 || len(o.ExcludePackage) > 0 {
			return errors.New("standalone TUN does not support user, package, or strict routing policies")
		}
	}
	return nil
}

func standaloneDNSPlan(options option.Options) (*StandaloneDNSPlan, error) {
	plan := &StandaloneDNSPlan{DNSMode: tun.DNSModeDisabled}
	for _, inbound := range options.Inbounds {
		if inbound.Type != C.TypeTun {
			continue
		}
		plan.HasTun = true
		o, ok := inbound.Options.(*option.TunInboundOptions)
		if !ok || o == nil {
			return nil, errors.New("invalid standalone TUN options")
		}
		// Match protocol/tun.NewInbound's family filtering and preserve order.
		tunOptions := tun.Options{
			Inet4Address: common.Filter(o.Address, func(p netip.Prefix) bool { return p.Addr().Is4() }),
			Inet6Address: common.Filter(o.Address, func(p netip.Prefix) bool { return p.Addr().Is6() }),
			AutoRoute:    o.AutoRoute, DNSMode: o.DNSMode, DNSAddress: o.DNSAddress,
		}
		mode := tunOptions.DNSModeOrDefault()
		switch mode {
		case tun.DNSModeDisabled:
			continue
		case tun.DNSModeNative, tun.DNSModeHijack:
		default:
			return nil, errors.New("invalid standalone DNS mode")
		}
		// The pinned Inet4DNSAddress error path indexes Inet6Address[0]. Guard
		// unusable single-address prefixes before invoking the upstream helpers.
		if len(o.DNSAddress) == 0 {
			has4 := len(tunOptions.Inet4Address) > 0 && tun.HasNextAddress(tunOptions.Inet4Address[0], 1)
			has6 := len(tunOptions.Inet6Address) > 0 && tun.HasNextAddress(tunOptions.Inet6Address[0], 1)
			if !has4 && !has6 {
				return nil, errors.New("standalone DNS requires a usable server address")
			}
		}
		servers, err := tunOptions.DNSServerAddress()
		if err != nil || len(servers) == 0 {
			return nil, errors.New("standalone DNS requires a usable server address")
		}
		// Explicit in-core DNS modes are validated even with manual routing:
		// the pinned inbound invokes the same helpers during Initialize. As in
		// NE, manual routing still leaves system resolver selection unchanged.
		if !o.AutoRoute {
			continue
		}
		if plan.DNSMode != tun.DNSModeDisabled {
			return nil, errors.New("multiple standalone system DNS configurations")
		}
		plan.DNSMode = mode
		plan.servers = common.Map(servers, netip.Addr.String)
	}
	return plan, nil
}

// standalonePlatformWrapper keeps native networking and opens native utun with
// add-only route ownership. An optional host supplies IPv6-aware path updates.
type standalonePlatformWrapper struct {
	platformInterfaceStub
	state      platformInterfaceWrapper
	host       StandalonePlatformInterface
	openTun    func(tun.Options) (tun.Tun, error)
	tunAccess  sync.Mutex
	interfaces []tun.Tun
}

var _ adapter.PlatformInterface = (*standalonePlatformWrapper)(nil)

func (w *standalonePlatformWrapper) Initialize(manager adapter.NetworkManager) error {
	// Box constructs the monitor before initializing the platform. Populate the
	// existing shared state rather than replacing a pointer captured by it.
	w.state.networkManager = manager
	return nil
}

func (w *standalonePlatformWrapper) UsePlatformAutoDetectInterfaceControl() bool { return false }
func (w *standalonePlatformWrapper) UsePlatformInterface() bool                  { return true }
func (w *standalonePlatformWrapper) UsePlatformDefaultInterfaceMonitor() bool    { return w.host != nil }

func (w *standalonePlatformWrapper) OpenInterface(options *tun.Options, _ option.TunPlatformOptions) (tun.Tun, error) {
	open := w.openTun
	if open == nil {
		open = openStandaloneTun
	}
	iface, err := open(*options)
	if iface != nil {
		w.tunAccess.Lock()
		w.interfaces = append(w.interfaces, iface)
		w.tunAccess.Unlock()
	}
	return iface, err
}

func (w *standalonePlatformWrapper) closeInterfaces() error {
	w.tunAccess.Lock()
	defer w.tunAccess.Unlock()
	var failure error
	for _, iface := range w.interfaces {
		if err := iface.Close(); err != nil {
			failure = errors.New("could not close standalone TUN")
		}
	}
	return failure
}

// Upstream Box.Start drains its cleanup scope on failure and discards close
// errors. Retain the owned wrappers so a later engine close still reports those
// sticky failures to CommandServer, without closing the native descriptor twice.
type standaloneEngine struct {
	mobileEngine
	platform  *standalonePlatformWrapper
	closeOnce sync.Once
	closeErr  error
}

func (e *standaloneEngine) Close() error {
	e.closeOnce.Do(func() {
		engineErr := e.mobileEngine.Close()
		tunErr := e.platform.closeInterfaces()
		if engineErr != nil || tunErr != nil {
			e.closeErr = errors.New("could not close standalone service")
		}
	})
	return e.closeErr
}

func (s *CommandServer) createEngine(ctx context.Context, platform PlatformInterface, options option.Options) (mobileEngine, error) {
	engine, err := s.newEngine(ctx, platform, options)
	if engine != nil && s.standalone {
		if wrapper, ok := service.FromContext[adapter.PlatformInterface](ctx).(*standalonePlatformWrapper); ok {
			engine = &standaloneEngine{mobileEngine: engine, platform: wrapper}
		}
	}
	return engine, err
}

func (w *standalonePlatformWrapper) CreateDefaultInterfaceMonitor(log logger.Logger) tun.DefaultInterfaceMonitor {
	return &platformDefaultInterfaceMonitor{
		platformInterfaceWrapper: &w.state, host: w.host, logger: log,
		registerInterface: w.host.RegisterMyInterface,
	}
}

func (s *CommandServer) baseContext() context.Context {
	ctx := mobileBaseContext(s.platform)
	if s.standalone || s.monitor != nil {
		ctx = service.ContextWith[adapter.PlatformInterface](ctx, &standalonePlatformWrapper{host: s.monitor})
	}
	return ctx
}

type standalonePreparationKey struct{}

type preparedStandaloneService struct {
	engine  mobileEngine
	ctx     context.Context
	options *option.Options
	routes  standaloneRoutePlan
}

// These pinned constructors allocate stacks whose cleanup is installed only by
// Initialize. Keep their existing disposable validation path: retaining that
// validation engine would require double initialization or upstream changes.
func needsDisposableValidation(options option.Options) bool {
	return common.Any(options.Endpoints, func(e option.Endpoint) bool {
		return e.Type == C.TypeWireGuard || e.Type == C.TypeOpenConnect || e.Type == C.TypeOpenVPNClient || e.Type == C.TypeOpenVPNServer
	})
}

// PrepareService constructs one standalone engine while leaving it inactive.
// Invalid options and constructor failures precede the host's connecting event.
// Preparation is bounded to an idle server; legacy reload ordering is unchanged.
func (s *CommandServer) PrepareService(config string, overrides *OverrideOptions) (*StandaloneDNSPlan, error) {
	s.operations.Lock()
	defer s.operations.Unlock()
	s.state.Lock()
	if !s.standalone || s.closed || s.engine != nil || s.prepared != nil {
		s.state.Unlock()
		return nil, errors.New("standalone service cannot be prepared")
	}
	generation := s.stopGeneration
	s.state.Unlock()
	ctx, cancel := context.WithCancel(s.baseContext())
	ctx = context.WithValue(ctx, standalonePreparationKey{}, true)
	s.state.Lock()
	if s.closed || generation != s.stopGeneration {
		s.state.Unlock()
		cancel()
		return nil, errors.New("standalone preparation was stopped")
	}
	s.cancel = cancel
	s.state.Unlock()
	var engine mobileEngine
	retained := false
	defer func() {
		if !retained {
			cancel()
			if engine != nil {
				_ = s.closeMobileEngine(engine)
			}
			s.state.Lock()
			s.cancel = nil
			s.state.Unlock()
		}
	}()
	options, err := parseConfig(ctx, config)
	if err != nil {
		return nil, errors.New("invalid standalone configuration")
	}
	if err = applyOverrides(&options, overrides); err != nil {
		return nil, err
	}
	if err = validateStandaloneOptions(options); err != nil {
		return nil, err
	}
	plan, err := standaloneDNSPlan(options)
	if err != nil {
		return nil, err
	}
	routes, err := buildStandaloneRoutePlan(options)
	if err != nil {
		return nil, err
	}
	prepared := &preparedStandaloneService{ctx: ctx, routes: routes}
	if needsDisposableValidation(options) {
		if err = CheckConfig(config); err != nil {
			return nil, errors.New("invalid standalone configuration")
		}
		prepared.options = &options
	} else {
		engine, err = s.createEngine(ctx, nil, options)
		if err != nil || engine == nil {
			return nil, errors.New("invalid standalone configuration")
		}
		prepared.engine = engine
	}
	s.state.Lock()
	defer s.state.Unlock()
	if s.closed || generation != s.stopGeneration || ctx.Err() != nil {
		return nil, errors.New("standalone preparation was stopped")
	}
	s.prepared = prepared
	retained = true
	return plan, nil
}

// ActivateService starts the exact engine retained by PrepareService. Permanent
// Close cancels both phases, including when it wins before this call enters Go.
func (s *CommandServer) ActivateService() error {
	s.operations.Lock()
	defer s.operations.Unlock()
	s.state.Lock()
	if !s.standalone || s.closed || s.prepared == nil {
		s.state.Unlock()
		return errors.New("standalone service is not prepared")
	}
	prepared := s.prepared
	s.state.Unlock()
	if prepared.options != nil {
		var err error
		prepared.engine, err = s.createEngine(prepared.ctx, nil, *prepared.options)
		if err != nil || prepared.engine == nil {
			_ = s.closeEngine()
			return errors.New("could not create standalone service")
		}
	}
	if prepared.ctx.Err() != nil {
		_ = s.closeEngine()
		return errors.New("standalone startup was stopped")
	}
	// Preparation/checkConfig performs no host-route inspection. Recheck at
	// activation so routes installed by another VPN since preparation are kept.
	if err := s.checkStandaloneRoutes(prepared.routes); err != nil {
		_ = s.closeEngine()
		return err
	}
	if err := prepared.engine.Start(); err != nil || prepared.ctx.Err() != nil || s.isClosed() {
		_ = s.closeEngine()
		return errors.New("could not start standalone service")
	}
	s.state.Lock()
	if s.closed || s.prepared != prepared {
		s.state.Unlock()
		_ = s.closeEngine()
		return errors.New("standalone startup was stopped")
	}
	s.engine, s.prepared = prepared.engine, nil
	s.state.Unlock()
	return nil
}
