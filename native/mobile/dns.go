// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2022 nekohasekai <contact-sagernet@sekai.icu>
// Adapted from SagerNet/sing-box v1.14.4; see NOTICE for the pinned revision.

package libbox

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"syscall"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"

	mDNS "github.com/miekg/dns"
)

type LocalDNSTransport interface {
	Raw() bool
	Lookup(ctx *ExchangeContext, network string, domain string) error
	Exchange(ctx *ExchangeContext, message []byte) error
}

type platformTransport struct {
	dns.TransportAdapter
	iif               LocalDNSTransport
	preferredResolver *local.PreferredDomainResolver
	networkManager    adapter.NetworkManager
}

func newPlatformTransport(ctx context.Context, logger log.ContextLogger, iif LocalDNSTransport, tag string, options option.LocalDNSServerOptions) (*platformTransport, error) {
	preferredResolver, err := local.NewPreferredDomainResolver(ctx, logger, options)
	if err != nil {
		return nil, err
	}
	return &platformTransport{
		TransportAdapter:  dns.NewTransportAdapterWithLocalOptions(C.DNSTypeLocal, tag, options),
		iif:               iif,
		preferredResolver: preferredResolver,
		networkManager:    service.FromContext[adapter.NetworkManager](ctx),
	}, nil
}

func (p *platformTransport) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	p.preferredResolver.Start(stage)
	return nil
}

func (p *platformTransport) Reset() {
}

func (p *platformTransport) PreferredDomain(domain string) bool {
	return p.preferredResolver.PreferredDomain(domain)
}

func (p *platformTransport) Environment() []string {
	if p.networkManager == nil {
		return nil
	}
	defaultInterface := p.networkManager.DefaultNetworkInterface()
	if defaultInterface == nil {
		return nil
	}
	return defaultInterface.DNSServers
}

func (p *platformTransport) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	if message == nil || len(message.Question) != 1 {
		return nil, E.New("DNS requires one question")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if localResponse := p.preferredResolver.Lookup(message); localResponse != nil {
		return localResponse, nil
	}
	response := &ExchangeContext{context: ctx}
	defer response.finish()
	done := make(chan error, 1)
	if p.iif.Raw() {
		messageBytes, err := message.Pack()
		if err != nil {
			return nil, err
		}
		go func() { done <- p.iif.Exchange(response, messageBytes) }()
	} else {
		var network string
		switch message.Question[0].Qtype {
		case mDNS.TypeA:
			network = "ip4"
		case mDNS.TypeAAAA:
			network = "ip6"
		default:
			return nil, E.New("platform DNS supports only IP queries")
		}
		go func() { done <- p.iif.Lookup(response, network, message.Question[0].Name) }()
	}
	select {
	case err := <-done:
		if err != nil {
			return nil, err
		}
		response.access.Lock()
		defer response.access.Unlock()
		if response.error != nil {
			return nil, response.error
		}
		if p.iif.Raw() {
			return response.message.Copy(), nil
		}
		return dns.FixedResponse(message.Id, message.Question[0], response.addresses, C.DefaultDNSTTL), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *platformTransport) ExchangeAsync(ctx context.Context, message *mDNS.Msg, callback func(response *mDNS.Msg, err error)) {
	go func() {
		callback(p.Exchange(ctx, message))
	}()
}

type Func interface {
	Invoke() error
}

type ExchangeContext struct {
	context       context.Context
	access        sync.Mutex
	message       mDNS.Msg
	addresses     []netip.Addr
	error         error
	finished      bool
	cancellations []dnsCancellation
}

type dnsCancellation struct {
	stop     func() bool
	callback Func
}

// context.AfterFunc does not allocate a goroutine until cancellation. Stopping
// registrations when the query finishes also releases captured Java callbacks.
func (c *ExchangeContext) OnCancel(callback Func) {
	if callback == nil {
		return
	}
	c.access.Lock()
	if c.finished {
		canceled := c.context.Err() != nil
		c.access.Unlock()
		if canceled {
			_ = callback.Invoke()
		}
		return
	}
	stop := context.AfterFunc(c.context, func() { _ = callback.Invoke() })
	c.cancellations = append(c.cancellations, dnsCancellation{stop: stop, callback: callback})
	c.access.Unlock()
}

func (c *ExchangeContext) finish() {
	c.access.Lock()
	c.finished = true
	canceled := c.context.Err() != nil
	registrations := c.cancellations
	c.cancellations = nil
	c.access.Unlock()
	for _, registration := range registrations {
		// A successful stop transfers invocation ownership to finish. Cancellation
		// may already be visible before AfterFunc has scheduled its callback.
		if registration.stop() && canceled {
			_ = registration.callback.Invoke()
		}
	}
}

func (c *ExchangeContext) Success(result string) {
	c.access.Lock()
	defer c.access.Unlock()
	if c.finished {
		return
	}
	for _, value := range strings.Split(result, "\n") {
		if value == "" {
			continue
		}
		address, err := netip.ParseAddr(value)
		if err != nil {
			c.error = E.New("invalid platform DNS response")
			return
		}
		c.addresses = append(c.addresses, address.Unmap())
	}
}

func (c *ExchangeContext) RawSuccess(result []byte) {
	c.access.Lock()
	defer c.access.Unlock()
	if c.finished {
		return
	}
	if err := c.message.Unpack(result); err != nil {
		c.error = E.New("invalid platform DNS response")
	}
}

func (c *ExchangeContext) ErrorCode(code int32) {
	c.access.Lock()
	defer c.access.Unlock()
	if !c.finished {
		c.error = dns.RcodeError(code)
	}
}

func (c *ExchangeContext) ErrnoCode(code int32) {
	c.access.Lock()
	defer c.access.Unlock()
	if !c.finished {
		c.error = syscall.Errno(code)
	}
}

var (
	_ adapter.DNSTransport                    = (*platformTransport)(nil)
	_ adapter.DNSTransportWithPreferredDomain = (*platformTransport)(nil)
	_ adapter.DNSTransportWithEnvironment     = (*platformTransport)(nil)
)
