// SPDX-License-Identifier: GPL-3.0-or-later
// The private Linux child-process protocol. Configuration travels only over
// inherited pipes; the helper opens no command socket and never logs requests.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	libbox "github.com/sagernet/sing-box/experimental/libbox"
	"golang.org/x/sys/unix"
)

type request struct {
	ID           int    `json:"id"`
	Method       string `json:"method"`
	Config       string `json:"config"`
	generation   uint64
	stopRecorded bool
}

type coreService interface {
	Start() error
	Close() error
}

type startConfig struct {
	native *libbox.DesktopConfig
	hasTun bool
}

func prepareStartConfig(content string) (startConfig, error) {
	config, err := libbox.ParseDesktopConfig(content)
	if err != nil {
		return startConfig{}, err
	}
	return startConfig{native: config, hasTun: config.HasTunInbound()}, nil
}

// Each startup owns its resources even after its RPC has been cancelled. A late
// constructor/Start return must never clean up a subsequent connection.
type ownedService struct {
	coreService
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (s *ownedService) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.err = s.coreService.Close()
	})
	return s.err
}

type startOperation struct {
	request   request
	parent    context.Context
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{} // RPC completion, including cancellation.
	finished  chan struct{} // Actual engine worker exit, which may be later.
	completed bool          // Outcome and publication are guarded by server.access.
	succeeded bool
	stopped   bool
}

type server struct {
	output         *json.Encoder
	access         sync.Mutex
	outputAccess   sync.Mutex // Never hold access during pipe I/O or engine calls.
	service        *ownedService
	startup        *startOperation
	stopGeneration uint64
	state          string
	create         func(context.Context, *libbox.DesktopConfig) (coreService, error)
	prepare        func(string) (startConfig, error)
	permission     func() bool
	initialized    bool
}

func newCoreService(ctx context.Context, config *libbox.DesktopConfig) (coreService, error) {
	instance, err := libbox.NewDesktopServiceWithConfig(ctx, config)
	if err != nil {
		// A nil concrete pointer must not become a non-nil coreService interface.
		return nil, err
	}
	return instance, nil
}

func main() {
	// A parent can close stdout while startup is in progress. A broken pipe
	// must reach the EOF cleanup path instead of terminating Go via SIGPIPE.
	signal.Ignore(syscall.SIGPIPE)
	dataDir := flag.String("data-dir", "", "private absolute state directory")
	flag.Parse()
	if err := prepareStateDirectory(*dataDir); err != nil {
		os.Exit(2)
	}
	if err := libbox.Setup(&libbox.SetupOptions{
		BasePath: *dataDir, WorkingPath: *dataDir, TempPath: filepath.Join(*dataDir, "tmp"),
	}); err != nil {
		os.Exit(2)
	}
	s := &server{
		output: json.NewEncoder(os.Stdout), state: "disconnected",
		create: newCoreService, prepare: prepareStartConfig, permission: tunPermission,
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	s.serve(os.Stdin, signals)
}

func (s *server) serve(input io.Reader, signals <-chan os.Signal) {
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		_ = s.close()
	}()
	requests := make(chan request)
	go func() {
		defer close(requests)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), 16*1024*1024)
		for scanner.Scan() {
			var r request
			if json.Unmarshal(scanner.Bytes(), &r) != nil || r.ID <= 0 {
				return
			}
			select {
			case requests <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	// Only one handler runs at a time, but the control loop keeps draining input
	// and observing shutdown while an engine operation is blocked. In particular,
	// a queued stop must not hide a following parent EOF from the reader.
	var pending []request
	var active <-chan struct{}
	for {
		if active == nil && len(pending) != 0 {
			r := pending[0]
			pending[0] = request{}
			pending = pending[1:]
			done := make(chan struct{})
			active = done
			go func() {
				defer close(done)
				s.handle(ctx, r)
			}()
		}
		select {
		case <-signals:
			return
		case <-active:
			active = nil
		case r, ok := <-requests:
			if !ok {
				return // Parent/engine exit must release the TUN and routes.
			}
			s.access.Lock()
			r.generation = s.stopGeneration
			if r.Method == "stop" {
				// Complete the interrupted RPC without waiting for Start to return.
				// The stop reply still follows it through the ordered request queue.
				s.interruptStartupLocked()
				r.stopRecorded = true
			}
			s.access.Unlock()
			pending = append(pending, r)
		}
	}
}

// Only the serialized request handler writes the protocol. Workers and the
// control loop commit startup outcomes under access, then wake that handler.
// A blocked parent output pipe must not prevent EOF/SIGTERM resource cleanup.
func (s *server) emit(state string) {
	s.outputAccess.Lock()
	defer s.outputAccess.Unlock()
	_ = s.output.Encode(map[string]any{"type": "status", "state": state})
}

func (s *server) setState(state string) {
	s.access.Lock()
	s.state = state
	s.access.Unlock()
	s.emit(state)
}

func (s *server) reply(r request, result any) {
	s.outputAccess.Lock()
	defer s.outputAccess.Unlock()
	_ = s.output.Encode(map[string]any{"id": r.ID, "result": result})
}

func (s *server) fail(r request, code, message string) {
	s.outputAccess.Lock()
	defer s.outputAccess.Unlock()
	_ = s.output.Encode(map[string]any{"id": r.ID, "error": map[string]string{"code": code, "message": message}})
}

func (s *server) close() error {
	s.access.Lock()
	instance, op := s.service, s.startup
	s.startup = nil
	if op != nil {
		op.cancel()
		if !op.completed {
			op.completed = true
			close(op.done)
		}
	}
	s.access.Unlock()
	var err error
	if instance != nil {
		// Retain ownership until Close finishes: a simultaneous EOF/SIGTERM
		// cleanup must wait for the same resource closure, not exit early.
		err = instance.Close()
		s.access.Lock()
		if s.service == instance {
			s.service = nil
		}
		s.access.Unlock()
	}
	if op != nil {
		// Give cancellable upstream operations time to finish their deferred
		// cleanup before reconnecting, without queuing stop behind an engine
		// that ignores cancellation. Real download cancellation is handled by
		// the Linux Clash source adapter, rather than abandoned HTTP workers.
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-op.finished:
		case <-timer.C:
		}
	}
	return err
}

func (s *server) interruptStartup() {
	s.access.Lock()
	defer s.access.Unlock()
	s.interruptStartupLocked()
}

func (s *server) interruptStartupLocked() {
	s.stopGeneration++
	if op := s.startup; op != nil && !op.completed {
		op.cancel()
		op.completed, op.stopped = true, true
		s.state = "error"
		close(op.done)
	}
}

func (s *server) beginStart(parent context.Context, r request) *startOperation {
	s.access.Lock()
	if parent.Err() != nil {
		s.access.Unlock()
		return nil
	}
	if r.generation != s.stopGeneration {
		s.access.Unlock()
		s.fail(r, "start_failed", "Linux service startup was stopped.")
		return nil
	}
	if s.service != nil || s.startup != nil {
		s.access.Unlock()
		s.fail(r, "already_started", "Stop the active connection before starting another.")
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	op := &startOperation{
		request: r, parent: parent, ctx: ctx, cancel: cancel,
		done: make(chan struct{}), finished: make(chan struct{}),
	}
	s.startup, s.state = op, "connecting"
	s.access.Unlock()
	return op
}

func (s *server) currentStartLocked(op *startOperation) bool {
	return s.startup == op && !op.completed && op.request.generation == s.stopGeneration && op.ctx.Err() == nil
}

func (s *server) publish(op *startOperation, instance *ownedService) bool {
	s.access.Lock()
	defer s.access.Unlock()
	if !s.currentStartLocked(op) {
		return false
	}
	s.service = instance
	return true
}

// finishStart is the only success commit. In particular it cannot publish
// connected between a cancellation check and a concurrently observed stop.
func (s *server) finishStart(op *startOperation, err error) bool {
	s.access.Lock()
	defer s.access.Unlock()
	if s.startup != op || op.completed || op.request.generation != s.stopGeneration || op.parent.Err() != nil {
		return false
	}
	if err == nil {
		err = op.ctx.Err()
	}
	op.completed, op.succeeded = true, err == nil
	s.startup = nil
	if err != nil {
		s.service, s.state = nil, "error"
		op.cancel()
	} else {
		s.state = "connected"
	}
	close(op.done)
	return op.succeeded
}

func (s *server) replyStart(op *startOperation) {
	// op's immutable outcome precedes done's close. The handler publishes it
	// before the next queued request, including stop, can emit anything.
	if op.parent.Err() != nil {
		return
	}
	if op.succeeded {
		s.emit("connected")
		s.reply(op.request, nil)
	} else {
		s.emit("error")
		message := "Could not start the Linux core. Check the configuration, TUN permissions and routing conflicts."
		if op.stopped {
			message = "Linux service startup was stopped."
		}
		s.fail(op.request, "start_failed", message)
	}
}

func (s *server) runStart(op *startOperation, config startConfig) {
	defer close(op.finished)
	if op.ctx.Err() != nil {
		return
	}
	instance, err := s.create(op.ctx, config.native)
	var owned *ownedService
	if instance != nil {
		owned = &ownedService{coreService: instance, cancel: op.cancel}
	} else if err == nil {
		err = errors.New("missing Linux service")
	}
	if err == nil {
		if s.publish(op, owned) {
			err = owned.Start()
		} else {
			err = context.Canceled
		}
	}
	if err != nil || op.ctx.Err() != nil {
		if owned != nil {
			_ = owned.Close()
		}
	}
	if !s.finishStart(op, err) && owned != nil {
		// This instance belongs only to op, even if a newer service is active.
		_ = owned.Close()
	}
}

func (s *server) handle(ctx context.Context, r request) {
	if ctx.Err() != nil {
		return
	}
	if r.Method == "getInfo" {
		s.reply(r, map[string]string{"version": libbox.Version(), "fingerprint": libbox.CoreFingerprint()})
		return
	}
	if r.Method == "initialize" {
		s.access.Lock()
		s.initialized = true
		s.access.Unlock()
		s.reply(r, nil)
		return
	}
	s.access.Lock()
	initialized := s.initialized
	s.access.Unlock()
	if !initialized {
		s.fail(r, "not_initialized", "Call initialize before controlling the Linux core.")
		return
	}
	switch r.Method {
	case "requestPermission":
		s.reply(r, s.permission())
	case "checkConfig":
		if libbox.CheckDesktopConfig(r.Config) != nil {
			s.fail(r, "invalid_config", "The Linux core rejected the configuration.")
		} else {
			s.reply(r, nil)
		}
	case "getStatus":
		s.access.Lock()
		state := s.state
		s.access.Unlock()
		s.reply(r, map[string]string{"type": "status", "state": state})
	case "start":
		s.access.Lock()
		hasService := s.service != nil || s.startup != nil
		s.access.Unlock()
		if hasService {
			s.fail(r, "already_started", "Stop the active connection before starting another.")
			return
		}
		config, err := s.prepare(r.Config)
		if err != nil {
			s.fail(r, "invalid_config", "The Linux core rejected the configuration.")
			return
		}
		if config.hasTun && !s.permission() {
			s.fail(r, "permission_denied", "Linux TUN requires access to /dev/net/tun and CAP_NET_ADMIN. See doc/linux-setup.md.")
			return
		}
		if op := s.beginStart(ctx, r); op != nil {
			// Completion belongs to the coordinator, not the engine call. Stop
			// can release this handler even if an upstream Start ignores ctx.
			s.emit("connecting")
			go s.runStart(op, config)
			<-op.done
			s.replyStart(op)
		}
	case "stop":
		if !r.stopRecorded {
			s.interruptStartup()
		}
		s.setState("disconnecting")
		if s.close() != nil {
			s.setState("error")
			s.fail(r, "stop_failed", "Could not fully close the Linux core.")
		} else {
			s.setState("disconnected")
			s.reply(r, nil)
		}
	default:
		s.fail(r, "not_implemented", "Unknown Linux core method.")
	}
}

// Probe the actual process permissions by creating an unnamed, nonpersistent
// TUN without assigning addresses or routes. Closing the fd removes it.
func tunPermission() bool {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("")
	if err != nil {
		return false
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	return unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr) == nil
}
