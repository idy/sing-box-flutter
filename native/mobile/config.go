// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"errors"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"
)

func parseConfig(ctx context.Context, content string) (option.Options, error) {
	options, err := json.UnmarshalExtendedContext[*option.Options](ctx, []byte(content))
	if err != nil || options == nil {
		return option.Options{}, errors.New("invalid mobile configuration")
	}
	if err = checkSelectedCapabilities(*options); err != nil {
		return option.Options{}, err
	}
	return *options, nil
}

func CheckConfig(content string) error {
	ctx, cancel := context.WithCancel(mobileBaseContext(nil))
	defer cancel()
	ctx = pause.WithDefaultManager(ctx)
	ctx = service.ContextWith[adapter.PlatformInterface](ctx, &platformInterfaceStub{})
	options, err := parseConfig(ctx, content)
	if err != nil {
		return err
	}
	if _, err = privateLogPath(ctx, options.Log); err != nil {
		return err
	}
	return checkConfig(ctx, options)
}

func checkConfig(ctx context.Context, options option.Options) error {
	ctx, cleanup := withConstructorCleanup(ctx)
	defer cleanup.Close()
	options.Log = &option.LogOptions{Disabled: true}
	instance, err := box.New(box.Options{Context: ctx, Options: options})
	if instance != nil {
		_ = instance.Close()
	}
	closeHistory(ctx)
	if err != nil {
		return errors.New("invalid mobile configuration")
	}
	return nil
}

// These pinned endpoint constructors allocate userspace stacks before box.New
// can return a box. Their Initialize stages only register idempotent cleanup
// (device.Close uses sync.Once or a locked nil check). Retain this owner through
// early startup failures, without wrapping endpoints and losing their concrete
// DNS types or optional routing interfaces.
func withConstructorCleanup(ctx context.Context) (context.Context, *adapter.Scope) {
	// Registration must still succeed when the caller cancels during a constructor.
	cleanup := adapter.NewScope(context.WithoutCancel(ctx), log.NewNOPFactory().Logger())
	ctx = service.ContextWith[adapter.EndpointRegistry](ctx, &constructorEndpointRegistry{
		EndpointRegistry: service.FromContext[adapter.EndpointRegistry](ctx),
		cleanup:          cleanup,
	})
	return ctx, cleanup
}

type constructorEndpointRegistry struct {
	adapter.EndpointRegistry
	cleanup *adapter.Scope
}

func (r *constructorEndpointRegistry) Create(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag, kind string, options any) (adapter.Endpoint, error) {
	endpoint, err := r.EndpointRegistry.Create(ctx, router, logger, tag, kind, options)
	if err != nil {
		return nil, err
	}
	if err = registerConstructorCleanup(r.cleanup, kind+"["+tag+"]", kind, endpoint); err != nil {
		return nil, err
	}
	return endpoint, nil
}

func HasTunInbound(content string) (bool, error) {
	options, err := parseConfig(mobileBaseContext(nil), content)
	if err != nil {
		return false, err
	}
	return hasTunInbound(options), nil
}

func hasTunInbound(options option.Options) bool {
	for _, inbound := range options.Inbounds {
		if inbound.Type == C.TypeTun {
			return true
		}
	}
	return false
}
