// SPDX-License-Identifier: GPL-3.0-or-later
// Package wgc adapts github.com/idy/wgc to sing-box's existing userspace stack.
package wgc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	wireguard "github.com/idy/wgc"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/common/dialer"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	wgstack "github.com/sagernet/sing-box/transport/wireguard"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// Options describes one WGC peer. Ordinary sing-box rules select between endpoints.
type Options struct {
	Address          []netip.Prefix  `json:"address"`
	PrivateKey       string          `json:"private_key"`
	PeerPublicKey    string          `json:"peer_public_key"`
	PeerEndpoint     netip.AddrPort  `json:"peer_endpoint"`
	PreSharedKey     string          `json:"pre_shared_key,omitempty"`
	MTU              uint32          `json:"mtu,omitempty"`
	Transport        string          `json:"transport,omitempty"`
	TransportOptions json.RawMessage `json:"transport_options,omitempty"`
}

func RegisterEndpoint(registry *endpoint.Registry) {
	endpoint.Register[Options](registry, "wgc", NewEndpoint)
}

var _ adapter.InterfaceUpdateListener = (*Endpoint)(nil)

type Endpoint struct {
	endpoint.Adapter
	ctx                 context.Context
	runCtx              context.Context
	options             Options
	privateKey, peerKey wireguard.Key
	psk                 *wireguard.Key
	factory             TransportFactory
	newDevice           func(wgstack.DeviceOptions) (wgstack.Device, error)
	binding             *rebindTransport
	updates             chan struct{}
	rebinders           sync.WaitGroup
	mu                  sync.Mutex
	closed              bool
	startDone           chan struct{}
	startErr            error
	cancel              context.CancelFunc
	stopScope           func() bool
	conn                *wireguard.Conn
	device              wgstack.Device
	dialer              N.Dialer
	receivers, senders  sync.WaitGroup
	sessions            sync.WaitGroup
	closeOnce           sync.Once
}

func NewEndpoint(ctx context.Context, _ adapter.Router, _ log.ContextLogger, tag string, options Options) (adapter.Endpoint, error) {
	private, err := parseKey(options.PrivateKey)
	if err != nil {
		return nil, errors.New("invalid WGC private key")
	}
	peer, err := parseKey(options.PeerPublicKey)
	if err != nil {
		return nil, errors.New("invalid WGC peer key")
	}
	peerAddress := options.PeerEndpoint.Addr().Unmap()
	if !options.PeerEndpoint.IsValid() || options.PeerEndpoint.Port() == 0 || options.PeerEndpoint.Addr().Zone() != "" || peerAddress.IsUnspecified() || peerAddress.IsMulticast() {
		return nil, errors.New("WGC peer needs a numeric unicast endpoint")
	}
	options.PeerEndpoint = netip.AddrPortFrom(peerAddress, options.PeerEndpoint.Port())
	if len(options.Address) == 0 || len(options.Address) > 16 {
		return nil, errors.New("WGC needs local IP addresses")
	}
	for _, address := range options.Address {
		if !address.IsValid() || address.Addr().Is4In6() {
			return nil, errors.New("invalid WGC local address")
		}
	}
	if options.MTU == 0 {
		options.MTU = 1280
	}
	if options.MTU < 576 || options.MTU > uint32(wireguard.MaximumDataSize) {
		return nil, errors.New("invalid WGC MTU")
	}
	var psk *wireguard.Key
	if options.PreSharedKey != "" {
		key, err := wireguard.KeyFromBase64(options.PreSharedKey)
		if err != nil || key.String() != options.PreSharedKey {
			return nil, errors.New("invalid WGC preshared key")
		}
		psk = &key
	}
	if (options.Transport == "" || options.Transport == "udp") && !validUDPOptions(options.TransportOptions) {
		return nil, errors.New("the default WGC UDP transport has no options")
	}
	factory := transportFactory(options.Transport)
	if factory == nil {
		return nil, errors.New("WGC transport is not registered")
	}
	options.Address = slices.Clone(options.Address)
	options.TransportOptions = bytes.Clone(options.TransportOptions)
	return &Endpoint{Adapter: endpoint.NewAdapter("wgc", tag, []string{N.NetworkTCP, N.NetworkUDP}, nil), ctx: ctx, options: options, privateKey: private, peerKey: peer, psk: psk, factory: factory, newDevice: wgstack.NewDevice}, nil
}

func parseKey(value string) (wireguard.Key, error) {
	key, err := wireguard.KeyFromBase64(value)
	if err != nil || key.IsZero() || key.String() != value {
		return wireguard.Key{}, errors.New("invalid WGC key")
	}
	return key, nil
}

func (e *Endpoint) Start(stage adapter.StartStage, scope *adapter.Scope) (err error) {
	if stage == adapter.StartStateInitialize {
		scope.Add(e.Close)
		return nil
	}
	if stage != adapter.StartStateStart {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return net.ErrClosed
	}
	if e.startDone != nil {
		done := e.startDone
		e.mu.Unlock()
		<-done
		e.mu.Lock()
		err = e.startErr
		e.mu.Unlock()
		return err
	}
	e.startDone = make(chan struct{})
	e.updates = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(e.ctx)
	e.cancel = cancel
	e.stopScope = context.AfterFunc(scope.Context(), cancel)
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.startErr = err; close(e.startDone); e.mu.Unlock() }()
	runCtx, err := e.startSession(ctx)
	if err != nil {
		cancel()
		return err
	}
	e.sessions.Add(1)
	go e.serve(ctx, runCtx)
	go func() { <-ctx.Done(); _ = e.Close() }()
	return nil
}

// Each packet-loop failure ends only this session. The endpoint lifetime is
// owned by the service context, its Scope, and Close.
func (e *Endpoint) startSession(ctx context.Context) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(C.StartTimeout, cancel)
	defer timer.Stop()
	slot, err := e.openTransport(runCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	transport := &rebindTransport{active: slot, changed: make(chan struct{}), request: func() { e.InterfaceUpdated(runCtx) }}
	conn, err := wireguard.NewConn(transport, wireguard.Config{
		StaticPrivateKey: &e.privateKey, DisableStats: true, SharedWriteQueue: true,
		MaxPeers: 1, ReadQueueSize: 128, WriteQueueSize: 128,
		PeerConfigLookup: func(key wireguard.Key) (*wireguard.PeerConfig, error) {
			if key != e.peerKey {
				return nil, nil
			}
			return &wireguard.PeerConfig{Addr: e.options.PeerEndpoint, PSK: e.psk, DisableAddrRoaming: true}, nil
		},
	})
	if err != nil {
		cancel()
		_ = transport.Close()
		return nil, errors.New("WGC connection startup failed")
	}
	device, err := e.newDevice(wgstack.DeviceOptions{Context: runCtx, Address: e.options.Address, MTU: e.options.MTU})
	if err == nil {
		err = device.Start()
	}
	if err != nil {
		cancel()
		_ = conn.Close()
		if device != nil {
			_ = device.Close()
		}
		return nil, errors.New("WGC userspace stack startup failed")
	}
	e.mu.Lock()
	if e.closed || runCtx.Err() != nil {
		e.mu.Unlock()
		cancel()
		_ = conn.Close()
		_ = device.Close()
		return nil, net.ErrClosed
	}
	e.runCtx = runCtx
	e.conn, e.device, e.binding = conn, device, transport
	// Use sing-box's resolver, including its normal DNS routing and candidate handling.
	e.dialer = dialer.NewResolveDialer(runCtx, device, false, "", adapter.DNSQueryOptions{}, 0)
	e.receivers.Add(1)
	e.senders.Add(1)
	e.rebinders.Add(1)
	go e.receive(runCtx, conn, device, cancel)
	go e.send(runCtx, conn, device, cancel)
	go e.watchInterfaces(runCtx, transport, conn)
	e.mu.Unlock()
	return runCtx, nil
}

func (e *Endpoint) serve(ctx, runCtx context.Context) {
	defer e.sessions.Done()
	for {
		<-runCtx.Done()
		e.closeSession()
		for {
			// Bound recovery attempts, including repeated failures to create a stack
			// or reopen its protected transport while the network is unavailable.
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			var err error
			runCtx, err = e.startSession(ctx)
			if err == nil {
				break
			}
		}
	}
}

func (e *Endpoint) closeSession() {
	e.mu.Lock()
	conn, device := e.conn, e.device
	e.dialer = nil
	e.mu.Unlock()
	_ = conn.Close()
	e.receivers.Wait()
	_ = device.Close()
	e.senders.Wait()
	e.rebinders.Wait()
}

func (e *Endpoint) receive(ctx context.Context, conn *wireguard.Conn, device wgstack.Device, cancel context.CancelFunc) {
	defer e.receivers.Done()
	defer cancel()
	for packet := range conn.ReadQueue() {
		if ctx.Err() == nil && len(packet.Data) != 0 {
			// The existing sing-box device copies the IP payload before Release.
			_, _ = device.Write([][]byte{packet.Data}, 0)
		}
		packet.Release()
	}
}

func (e *Endpoint) send(ctx context.Context, conn *wireguard.Conn, device wgstack.Device, cancel context.CancelFunc) {
	defer e.senders.Done()
	defer cancel()
	buffers, sizes := [][]byte{make([]byte, e.options.MTU)}, []int{0}
	for {
		count, err := device.Read(buffers, sizes, 0)
		if err != nil {
			cancel()
			// A broken reader cannot drain the stack, even during shutdown.
			// Close it before joining any receiver blocked injecting a packet.
			_ = device.Close()
			return
		}
		// Continue draining during shutdown until the receiver releases its stack writes.
		if count > 0 && ctx.Err() == nil {
			_, _, err = conn.WriteTo(buffers[0][:sizes[0]], e.peerKey)
			if err != nil && !errors.Is(err, wireguard.ErrPeerNotFound) && !errors.Is(err, wireguard.ErrPeerBlocked) {
				cancel()
			}
		}
	}
}

func (e *Endpoint) current() (N.Dialer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.dialer == nil || e.runCtx.Err() != nil {
		return nil, net.ErrClosed
	}
	return e.dialer, nil
}

func (e *Endpoint) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	d, err := e.current()
	if err != nil {
		return nil, err
	}
	return d.DialContext(ctx, network, destination)
}

func (e *Endpoint) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	d, err := e.current()
	if err != nil {
		return nil, err
	}
	return d.ListenPacket(ctx, destination)
}

func (e *Endpoint) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		if e.cancel != nil {
			e.cancel()
		}
		if e.stopScope != nil {
			e.stopScope()
		}
		done := e.startDone
		e.mu.Unlock()
		if done != nil {
			<-done
		}
		e.sessions.Wait()
	})
	return nil
}
