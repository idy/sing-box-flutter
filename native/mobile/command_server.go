// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"errors"
	"sync"

	"github.com/sagernet/sing-box/option"
)

// CommandServer is the legacy binding name for the in-process VPN lifecycle.
// It has no socket, RPC listener, remote control API, or background controller.
type CommandServer struct {
	operations     sync.Mutex
	state          sync.Mutex
	closed         bool
	stopGeneration uint64
	cancel         context.CancelFunc
	engine         mobileEngine
	platform       PlatformInterface
	newEngine      func(context.Context, PlatformInterface, option.Options) (mobileEngine, error)
}

// Retained in full so the existing Swift/Kotlin implementations remain source
// compatible. No management callbacks are invoked by this in-process core.
type CommandServerHandler interface {
	ServiceStop() error
	ServiceReload() error
	GetSystemProxyStatus() (*SystemProxyStatus, error)
	SetSystemProxyEnabled(enabled bool) error
	TriggerNativeCrash() error
	WriteDebugMessage(message string)
	ConnectSSHAgent() (int32, error)
}

type SystemProxyStatus struct{ Available, Enabled bool }

type OverrideOptions struct {
	AutoRedirect   bool
	IncludePackage StringIterator
	ExcludePackage StringIterator
}

type mobileEngine interface {
	Start() error
	Close() error
	Pause()
	Wake()
	NeedWIFIState() bool
}

func NewCommandServer(handler CommandServerHandler, platform PlatformInterface) (*CommandServer, error) {
	if handler == nil || platform == nil {
		return nil, errors.New("mobile handler and platform are required")
	}
	return &CommandServer{platform: platform, newEngine: newBoxEngine}, nil
}

// Start is optional and idempotent. It never opens a command listener.
func (s *CommandServer) Start() error {
	s.state.Lock()
	defer s.state.Unlock()
	if s.closed {
		return errors.New("mobile core is closed")
	}
	return nil
}

func (s *CommandServer) StartOrReloadService(config string, options *OverrideOptions) error {
	s.operations.Lock()
	defer s.operations.Unlock()
	s.state.Lock()
	if s.closed {
		s.state.Unlock()
		return errors.New("mobile core is closed")
	}
	generation := s.stopGeneration
	s.state.Unlock()
	// Decode before stopping a healthy instance. Registries must be exactly the
	// generated selection both here and in CheckConfig; never import include.
	ctx := mobileBaseContext(s.platform)
	configuration, err := parseConfig(ctx, config)
	if err != nil {
		return errors.New("invalid mobile configuration")
	}
	if err = applyOverrides(&configuration, options); err != nil {
		return err
	}
	if err = s.closeEngine(); err != nil {
		return errors.New("could not stop mobile service")
	}
	ctx, cancel := context.WithCancel(ctx)
	s.state.Lock()
	if s.closed || generation != s.stopGeneration {
		s.state.Unlock()
		cancel()
		return errors.New("mobile service startup was stopped")
	}
	s.cancel = cancel
	s.state.Unlock()
	engine, err := s.newEngine(ctx, s.platform, configuration)
	if err == nil && engine == nil {
		err = errors.New("missing mobile engine")
	}
	if err == nil {
		err = engine.Start()
	}
	if err != nil || ctx.Err() != nil || s.isClosed() {
		cancel()
		if engine != nil {
			_ = engine.Close()
		}
		s.state.Lock()
		s.cancel = nil
		s.state.Unlock()
		return errors.New("could not start mobile service")
	}
	s.state.Lock()
	// Close marks closed and cancels without waiting on operations. Publishing
	// the engine is atomic with that transition; the closer owns it afterwards.
	if s.closed {
		s.state.Unlock()
		cancel()
		_ = engine.Close()
		return errors.New("mobile core is closed")
	}
	s.engine = engine
	s.state.Unlock()
	return nil
}

func applyOverrides(options *option.Options, overrides *OverrideOptions) error {
	if overrides == nil {
		return nil
	}
	// Mobile VPN APIs do not support Linux auto-redirect. Reject it rather than
	// silently replacing routing policy supplied in the config.
	if overrides.AutoRedirect {
		return errors.New("mobile auto-redirect is unsupported")
	}
	include := iteratorToArray[string](overrides.IncludePackage)
	exclude := iteratorToArray[string](overrides.ExcludePackage)
	for _, inbound := range options.Inbounds {
		if tun, ok := inbound.Options.(*option.TunInboundOptions); ok {
			tun.IncludePackage = append(tun.IncludePackage, include...)
			tun.ExcludePackage = append(tun.ExcludePackage, exclude...)
			return nil
		}
	}
	if len(include) != 0 || len(exclude) != 0 {
		return errors.New("package overrides require a TUN inbound")
	}
	return nil
}

func (s *CommandServer) CloseService() error {
	// Interrupt an in-flight Start before acquiring the serialization lock.
	s.state.Lock()
	if s.closed {
		s.state.Unlock()
		return errors.New("mobile core is closed")
	}
	s.stopGeneration++
	if s.cancel != nil {
		s.cancel()
	}
	s.state.Unlock()
	s.operations.Lock()
	defer s.operations.Unlock()
	if s.isClosed() {
		return errors.New("mobile core is closed")
	}
	if err := s.closeEngine(); err != nil {
		return errors.New("could not stop mobile service")
	}
	return nil
}

// Close preserves the no-result signature expected by Swift. It waits for all
// resources, is idempotent, and permanently prevents another startup.
func (s *CommandServer) Close() {
	s.state.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.state.Unlock()
	s.operations.Lock()
	defer s.operations.Unlock()
	_ = s.closeEngine()
}

func (s *CommandServer) closeEngine() error {
	s.state.Lock()
	engine, cancel := s.engine, s.cancel
	s.engine, s.cancel = nil, nil
	s.state.Unlock()
	if cancel != nil {
		cancel()
	}
	if engine != nil {
		return engine.Close()
	}
	return nil
}

func (s *CommandServer) isClosed() bool {
	s.state.Lock()
	defer s.state.Unlock()
	return s.closed
}

// Void/bool binding methods cannot report a closed-state error. They perform no
// work after Close and never resurrect resources.
func (s *CommandServer) Pause() {
	s.operations.Lock()
	defer s.operations.Unlock()
	if !s.isClosed() && s.engine != nil {
		s.engine.Pause()
	}
}

func (s *CommandServer) Wake() {
	s.operations.Lock()
	defer s.operations.Unlock()
	if !s.isClosed() && s.engine != nil {
		s.engine.Wake()
	}
}

func (s *CommandServer) NeedWIFIState() bool {
	s.operations.Lock()
	defer s.operations.Unlock()
	return !s.isClosed() && s.engine != nil && s.engine.NeedWIFIState()
}
