package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"time"

	"github.com/idy/sing-box-flutter/core/transport"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/common/dialer"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

const TypeWGC = "wgc"

// EndpointOptions embeds one legacy v1 WGC configuration. Peer defaults to the
// normalized DefaultPeer; Resolver defaults to the sing-box tag dns-remote.
// Network settings in Configuration never create a second VPN/TUN runtime.
type EndpointOptions struct {
	Configuration Config `json:"configuration"`
	Peer          string `json:"peer,omitempty"`
	Resolver      string `json:"resolver,omitempty"`
}

func (o *EndpointOptions) UnmarshalJSON(raw []byte) error {
	// Exclude untrusted field names, private options and values from decoder errors.
	type plainOptions EndpointOptions
	var decoded plainOptions
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > MaxConfigBytes+1024 || decoder.Decode(&decoded) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid WGC endpoint options")
	}
	*o = EndpointOptions(decoded)
	return nil
}

// RegisterEndpoint adds WGC to the normal sing-box endpoint/options registry.
func RegisterEndpoint(registry *endpoint.Registry) {
	endpoint.Register[EndpointOptions](registry, TypeWGC, NewEndpoint)
}

type sessionOptionsKey struct{}

// WithSessionOptions supplies compiled-in transport extensions and an optional
// additional peer admission policy. Without it, endpoints use transport.Default.
// The registry and policy must remain valid until the endpoint scope is closed.
func WithSessionOptions(ctx context.Context, options SessionOptions) context.Context {
	return context.WithValue(ctx, sessionOptionsKey{}, options)
}

var (
	_ adapter.Endpoint                   = (*Endpoint)(nil)
	_ dialer.PacketDialerWithDestination = (*Endpoint)(nil)
)

// Endpoint owns an encrypted WGC session and userspace peer stacks, while libbox
// owns the sole OS VPN/TUN. Construction and initialization are offline.
type Endpoint struct {
	endpoint.Adapter
	ctx      context.Context
	config   Config
	options  SessionOptions
	peer     string
	resolver string

	mu            sync.Mutex
	initialized   bool
	closed        bool
	session       *Session
	cancel        context.CancelFunc
	stopContext   func() bool
	stopScope     func() bool
	startTimer    *time.Timer
	startTimeout  time.Duration
	startDone     chan struct{}
	startFinished bool
	startErr      error
	closeOnce     sync.Once
}

// NewEndpoint validates and snapshots options without constructing a Session,
// opening a transport, querying DNS or requiring runtime platform services.
func NewEndpoint(ctx context.Context, _ adapter.Router, _ log.ContextLogger, tag string, options EndpointOptions) (adapter.Endpoint, error) {
	// Round-trip bounds the payload and takes ownership of all slices, raw options
	// and nested wrapper specs; callers may subsequently reuse their config.
	raw, err := json.Marshal(options.Configuration)
	if err != nil {
		return nil, errors.New("invalid WGC configuration")
	}
	config, err := DecodeConfig(raw)
	if err != nil {
		return nil, err
	}
	sessionOptions, _ := ctx.Value(sessionOptionsKey{}).(SessionOptions)
	if sessionOptions.Registry == nil {
		sessionOptions.Registry = transport.Default
	}
	validationCtx := endpointProtectorContext(ctx)
	if err := sessionOptions.Registry.ValidateContext(validationCtx, config.Transport); err != nil {
		return nil, errors.New("invalid WGC transport configuration")
	}
	peer := options.Peer
	if peer == "" {
		peer = config.DefaultPeer
	}
	declared := false
	for _, candidate := range config.Peers {
		if candidate.ID == peer {
			declared = true
			break
		}
	}
	if !declared {
		return nil, errors.New("WGC endpoint peer is not declared")
	}
	resolver := options.Resolver
	if resolver == "" {
		resolver = "dns-remote"
	}
	return &Endpoint{
		Adapter: endpoint.NewAdapter(TypeWGC, tag, []string{N.NetworkTCP, N.NetworkUDP}, nil),
		ctx:     ctx, config: config, options: sessionOptions, peer: peer, resolver: resolver,
		startTimeout: C.StartTimeout,
	}, nil
}

func endpointProtectorContext(ctx context.Context) context.Context {
	platform := service.FromContext[adapter.PlatformInterface](ctx)
	if platform != nil && (runtime.GOOS == "android" || platform.UsePlatformAutoDetectInterfaceControl()) {
		return transport.WithSocketProtector(ctx, platform.AutoDetectInterfaceControl)
	}
	return ctx
}

func (e *Endpoint) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	if stage != adapter.StartStateInitialize && stage != adapter.StartStateStart {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return net.ErrClosed
	}
	if !e.initialized {
		e.initialized = true
		scope.Add(e.Close)
	}
	if stage == adapter.StartStateInitialize {
		e.mu.Unlock()
		return nil
	}
	if e.startDone == nil {
		e.startDone = make(chan struct{})
		runCtx, cancel := context.WithCancel(e.ctx)
		e.cancel = cancel
		e.stopContext = context.AfterFunc(e.ctx, func() { e.abortStart(e.ctx.Err()); _ = e.Close() })
		e.stopScope = context.AfterFunc(scope.Context(), func() { e.abortStart(scope.Context().Err()); _ = e.Close() })
		e.startTimer = time.AfterFunc(e.startTimeout, func() { e.abortStart(context.DeadlineExceeded) })
		go e.openSession(endpointProtectorContext(runCtx))
	}
	done := e.startDone
	e.mu.Unlock()
	<-done
	e.mu.Lock()
	err := e.startErr
	e.mu.Unlock()
	return err
}

// abortStart releases every Start waiter even if a custom factory is still
// returning. openSession retains responsibility for closing any late session.
func (e *Endpoint) abortStart(err error) {
	e.mu.Lock()
	if e.startFinished || e.startDone == nil {
		e.mu.Unlock()
		return
	}
	e.finishStartLocked(err)
	e.closed = true
	e.mu.Unlock()
	_ = e.Close()
}

func (e *Endpoint) finishStartLocked(err error) {
	e.startFinished = true
	e.startErr = err
	if e.startTimer != nil {
		e.startTimer.Stop()
	}
	close(e.startDone)
}

func (e *Endpoint) openSession(ctx context.Context) {
	var session *Session
	var err error
	// Android must use libbox's platform callback, including for custom transports.
	if runtime.GOOS == "android" && service.FromContext[adapter.PlatformInterface](ctx) == nil {
		err = transport.ErrSocketProtectorRequired
	} else {
		session, err = NewSession(ctx, e.config, e.options)
	}
	if err != nil {
		switch {
		case ctx.Err() != nil:
			err = ctx.Err()
		case errors.Is(err, transport.ErrSocketProtectorRequired):
			err = transport.ErrSocketProtectorRequired
		default:
			err = errors.New("WGC endpoint startup failed")
		}
	} else if ctx.Err() != nil {
		err = ctx.Err()
	} else if session.ctx.Err() != nil {
		err = errors.New("WGC endpoint session stopped during startup")
	}
	e.mu.Lock()
	if e.closed || e.startFinished {
		e.mu.Unlock()
		if session != nil {
			_ = session.Close()
		}
		return
	}
	if err == nil {
		e.session = session
	} else {
		e.closed = true
	}
	e.finishStartLocked(err)
	e.mu.Unlock()
	if err != nil {
		if session != nil {
			_ = session.Close()
		}
		_ = e.Close()
	}
}

// Close cancels startup and in-flight operations before joining an active
// session. It is idempotent, including when racing Start or scope cancellation.
func (e *Endpoint) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		if e.cancel != nil {
			e.cancel()
		}
		if e.stopContext != nil {
			e.stopContext()
		}
		if e.stopScope != nil {
			e.stopScope()
		}
		if e.startDone != nil && !e.startFinished {
			e.finishStartLocked(net.ErrClosed)
		}
		session := e.session
		e.mu.Unlock()
		if session != nil {
			_ = session.Close()
		}
	})
	return nil
}

func (e *Endpoint) outbound(ctx context.Context) (*peerOutbound, context.Context, func(), error) {
	e.mu.Lock()
	session := e.session
	ready := !e.closed && session != nil && session.ctx.Err() == nil
	e.mu.Unlock()
	if !ready {
		return nil, nil, nil, net.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	operationCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(session.ctx, cancel)
	cleanup := func() { stop(); cancel() }
	return &peerOutbound{ctx: e.ctx, session: session, peer: e.peer, resolver: e.resolver}, operationCtx, cleanup, nil
}

func (e *Endpoint) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	switch N.NetworkName(network) {
	case N.NetworkTCP, N.NetworkUDP:
	default:
		return nil, errors.New("unsupported WGC network")
	}
	outbound, operationCtx, cleanup, err := e.outbound(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return outbound.DialContext(operationCtx, network, destination)
}

func (e *Endpoint) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	conn, _, err := e.ListenPacketWithDestination(ctx, destination)
	return conn, err
}

func (e *Endpoint) ListenPacketWithDestination(ctx context.Context, destination M.Socksaddr) (net.PacketConn, netip.Addr, error) {
	outbound, operationCtx, cleanup, err := e.outbound(ctx)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	defer cleanup()
	return outbound.ListenPacketWithDestination(operationCtx, destination)
}
