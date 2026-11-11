// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sync"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
)

var errConfig = errors.New("invalid Windows configuration")

type boxBackend struct{}

func (boxBackend) Version() string { return C.Version }

func parseConfig(ctx context.Context, content string, requireTUN bool) (option.Options, error) {
	options, err := json.UnmarshalExtendedContext[*option.Options](ctx, []byte(content))
	if err != nil || options == nil {
		return option.Options{}, errConfig
	}
	if err = checkSelectedCapabilities(*options); err != nil {
		return option.Options{}, errConfig
	}
	foundTUN := false
	for index := range options.Inbounds {
		inbound := &options.Inbounds[index]
		if inbound.Type != C.TypeTun {
			continue
		}
		foundTUN = true
		tunOptions, ok := inbound.Options.(*option.TunInboundOptions)
		if !ok || tunOptions == nil || validateTunDNS(tunOptions) != nil {
			return option.Options{}, errConfig
		}
		// All supported packet stacks use the native Wintun device. Preserve
		// explicit stack and routing choices; use the system stack by default.
		if tunOptions.Stack == "" {
			tunOptions.Stack = "system"
		}
	}
	if requireTUN && !foundTUN {
		return option.Options{}, errConfig
	}
	options.Log = &option.LogOptions{Disabled: true}
	if options.Experimental != nil {
		// Profiling/debug options can dump process state or open diagnostic
		// listeners independently of the log factory; disable that entire path.
		options.Experimental.Debug = nil
	}
	return *options, nil
}

func validateTunDNS(options *option.TunInboundOptions) error {
	// Match protocol/tun.NewInbound and the standalone DNS preflight. In the
	// pinned sing-tun, Inet4DNSAddress's error path indexes Inet6Address[0].
	// Reject unusable derived addresses before Box construction or Initialize.
	dns := tun.Options{
		Inet4Address: common.Filter(options.Address, func(p netip.Prefix) bool { return p.Addr().Is4() }),
		Inet6Address: common.Filter(options.Address, func(p netip.Prefix) bool { return p.Addr().Is6() }),
		AutoRoute:    options.AutoRoute, DNSMode: options.DNSMode, DNSAddress: options.DNSAddress,
	}
	for _, address := range options.Address {
		if !address.IsValid() {
			return errConfig
		}
	}
	for _, address := range options.DNSAddress {
		if !address.IsValid() || address.IsUnspecified() || address.IsMulticast() || address.Zone() != "" {
			return errConfig
		}
	}
	switch dns.DNSModeOrDefault() {
	case tun.DNSModeDisabled:
		return nil
	case tun.DNSModeNative, tun.DNSModeHijack:
	default:
		return errConfig
	}
	if len(dns.DNSAddress) == 0 {
		has4 := len(dns.Inet4Address) > 0 && tun.HasNextAddress(dns.Inet4Address[0], 1)
		has6 := len(dns.Inet6Address) > 0 && tun.HasNextAddress(dns.Inet6Address[0], 1)
		if !has4 && !has6 {
			return errConfig
		}
	}
	servers, err := dns.DNSServerAddress()
	if err != nil || len(servers) == 0 {
		return errConfig
	}
	for _, address := range servers {
		if !address.IsValid() || address.IsUnspecified() || address.IsMulticast() || address.Zone() != "" {
			return errConfig
		}
	}
	return nil
}

func engineContext(parent context.Context, working string) context.Context {
	// baseContext is generated from the exact host selection. Its registries
	// are never replaced with the upstream all-features include package.
	return filemanager.WithDefault(baseContext(parent), working, filepath.Join(working, "tmp"), os.Getuid(), os.Getgid())
}

func (boxBackend) Check(ctx context.Context, content, working string) error {
	instance, err := createBox(ctx, content, working, false)
	if instance != nil {
		_ = instance.Close()
	}
	if err != nil {
		return errConfig
	}
	return nil
}

func (boxBackend) New(ctx context.Context, content, working string) (core, error) {
	instance, err := createBox(ctx, content, working, true)
	if err != nil {
		return nil, err
	}
	return instance, nil
}

type boxCore struct {
	box          *box.Box
	ctx          context.Context
	constructors *adapter.Scope
	storage      io.Closer
	closeOnce    sync.Once
	closeErr     error
}

func createBox(parent context.Context, content, working string, requireTUN bool) (*boxCore, error) {
	ctx := engineContext(parent, working)
	options, err := parseConfig(ctx, content, requireTUN)
	if err != nil {
		return nil, errConfig
	}
	storage, err := prepareEngineStorage(ctx, options)
	if err != nil {
		return nil, errConfig
	}
	// Box construction allocates some endpoint stacks before it has a lifecycle
	// scope. Register their cleanup immediately, including on later constructor
	// failure, without starting any clients, listeners, or network monitors.
	e := &boxCore{ctx: ctx, storage: storage, constructors: adapter.NewScope(context.WithoutCancel(ctx), log.NewNOPFactory().Logger())}
	ctx = service.ContextWith[adapter.EndpointRegistry](ctx, &constructorRegistry{
		EndpointRegistry: service.FromContext[adapter.EndpointRegistry](ctx), cleanup: e.constructors,
	})
	e.box, err = box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		_ = e.Close()
		return nil, errConfig
	}
	return e, nil
}

func (e *boxCore) Start() error {
	if err := e.box.Start(); err != nil {
		return errors.New("could not start Windows tunnel")
	}
	return nil
}
func (e *boxCore) Close() error {
	e.closeOnce.Do(func() {
		if e.box != nil {
			e.closeErr = e.box.Close()
		}
		if err := e.constructors.Close(); e.closeErr == nil {
			e.closeErr = err
		}
		if history := service.PtrFromContext[urltest.HistoryStorage](e.ctx); history != nil {
			history.Close()
		}
		// Native cache IO and endpoint cleanup must finish before their paths
		// can be renamed or replaced, including after constructor failure.
		if e.storage != nil {
			if err := e.storage.Close(); e.closeErr == nil {
				e.closeErr = err
			}
		}
	})
	if e.closeErr != nil {
		return errors.New("could not close Windows tunnel")
	}
	return nil
}

type constructorRegistry struct {
	adapter.EndpointRegistry
	cleanup *adapter.Scope
}

func (r *constructorRegistry) Create(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag, kind string, options any) (adapter.Endpoint, error) {
	endpoint, err := r.EndpointRegistry.Create(ctx, router, logger, tag, kind, options)
	if err != nil {
		return nil, err
	}
	if err = registerConstructorCleanup(r.cleanup, kind+"["+tag+"]", kind, endpoint); err != nil {
		return nil, err
	}
	return endpoint, nil
}
