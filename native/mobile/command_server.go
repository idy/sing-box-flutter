// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"errors"
	"runtime"
	"sync"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

// CommandServer is the legacy binding name for the in-process VPN lifecycle.
// It has no socket, RPC listener, remote control API, or background controller.
type CommandServer struct {
	operations     sync.Mutex
	state          sync.Mutex
	closed         bool
	closeErr       error // Sanitized, sticky result; failed cleanup requires a new process.
	stopGeneration uint64
	cancel         context.CancelFunc
	engine         mobileEngine
	prepared       *preparedStandaloneService
	standalone     bool
	monitor        StandalonePlatformInterface
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

// NewStandaloneCommandServer uses native macOS utun networking without a Network
// Extension or host platform callbacks. Service startup requires the host to have
// the permissions needed by sing-tun; construction does not open a tunnel.
func NewStandaloneCommandServer() (*CommandServer, error) {
	if runtime.GOOS != "darwin" || C.IsIos {
		return nil, errors.New("standalone core requires macOS")
	}
	return &CommandServer{standalone: true, newEngine: newBoxEngine}, nil
}

// NewStandaloneCommandServerWithMonitor keeps native networking while accepting
// an IPv6-aware host path monitor. The unmonitored constructor remains useful
// for non-VPN standalone services and lifecycle smoke tests.
func NewStandaloneCommandServerWithMonitor(monitor StandalonePlatformInterface) (*CommandServer, error) {
	if monitor == nil {
		return nil, errors.New("standalone interface monitor is required")
	}
	server, err := NewStandaloneCommandServer()
	if err != nil {
		return nil, err
	}
	server.monitor = monitor
	return server, nil
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
	ctx := s.baseContext()
	configuration, err := parseConfig(ctx, config)
	if err != nil {
		return errors.New("invalid mobile configuration")
	}
	if err = applyOverrides(&configuration, options); err != nil {
		return err
	}
	if s.standalone {
		if err = validateStandaloneOptions(configuration); err != nil {
			return err
		}
		if _, err = standaloneDNSPlan(configuration); err != nil {
			return err
		}
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
			_ = s.closeMobileEngine(engine)
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
		_ = s.closeMobileEngine(engine)
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
		err := s.closeErr
		s.state.Unlock()
		if err != nil {
			return err
		}
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
		s.state.Lock()
		err := s.closeErr
		s.state.Unlock()
		if err != nil {
			return err
		}
		return errors.New("mobile core is closed")
	}
	return s.closeEngine()
}

// Close preserves the void ABI used by existing Android/iOS callers. Call
// CloseWithError when the host must know whether permanent teardown succeeded.
func (s *CommandServer) Close() {
	_ = s.CloseWithError()
}

// CloseWithError cancels before waiting for an in-flight operation, permanently
// prevents startup and drains all owned engines exactly once. Every call waits
// for cleanup and returns the same sanitized failure, including failures from
// partial startup or a prior CloseService. Failed cleanup is never retried: the
// engine may already have partially released resources and requires process exit.
func (s *CommandServer) CloseWithError() error {
	s.state.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.state.Unlock()
	s.operations.Lock()
	defer s.operations.Unlock()
	return s.closeEngine()
}

func (s *CommandServer) closeEngine() error {
	s.state.Lock()
	engine, prepared, cancel := s.engine, s.prepared, s.cancel
	s.engine, s.prepared, s.cancel = nil, nil, nil
	s.state.Unlock()
	if cancel != nil {
		cancel()
	}
	if engine != nil {
		_ = s.closeMobileEngine(engine)
	}
	if prepared != nil && prepared.engine != nil {
		_ = s.closeMobileEngine(prepared.engine)
	}
	s.state.Lock()
	defer s.state.Unlock()
	return s.closeErr
}

// closeMobileEngine is called by the serialized owner, including partial
// startup cleanup. Retain only a fixed message: native errors may contain
// credentials or configuration. Failure poisons the server even if the engine
// pointer has already been removed, so a later no-op close cannot hide it.
func (s *CommandServer) closeMobileEngine(engine mobileEngine) error {
	if err := engine.Close(); err != nil {
		s.state.Lock()
		defer s.state.Unlock()
		if s.closeErr == nil {
			s.closeErr = errors.New("could not stop mobile service cleanly; restart the process")
		}
		s.closed = true
		return s.closeErr
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
