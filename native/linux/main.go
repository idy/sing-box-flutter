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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
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
	cleanupFailed  bool // Terminal: only replacing this helper can make cleanup trustworthy.
	shuttingDown   bool
	workers        sync.WaitGroup // Includes cancelled and retired startup workers.
	state          string
	create         func(context.Context, *libbox.DesktopConfig) (coreService, error)
	prepare        func(string) (startConfig, error)
	permission     func() bool
	initialized    bool
}

const cleanupFailureMessage = "Could not fully close the Linux core. Replace the helper before reconnecting."

var errCleanupFailed = errors.New(cleanupFailureMessage)

func newCoreService(ctx context.Context, config *libbox.DesktopConfig) (coreService, error) {
	instance, err := libbox.NewDesktopServiceWithConfig(ctx, config)
	if err != nil {
		// A nil concrete pointer must not become a non-nil coreService interface.
		return nil, err
	}
	return instance, nil
}

func main() {
	os.Exit(run())
}

func run() int {
	// A parent can close stdout while startup is in progress. A broken pipe
	// must reach the EOF cleanup path instead of terminating Go via SIGPIPE.
	signal.Ignore(syscall.SIGPIPE)
	dataDir := flag.String("data-dir", "", "private absolute state directory")
	temporary := flag.Bool("temporary-state", false, "remove owned temporary state after shutdown")
	worker := flag.Bool("temporary-state-worker", false, "internal supervised temporary-state worker")
	supervisorPID := flag.Int("supervisor-pid", 0, "internal temporary-state supervisor identity")
	flag.Parse()
	if *worker {
		if !*temporary || bindTemporaryWorker(*supervisorPID) != nil {
			return 2
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	statePath, err := prepareStateDirectory(*dataDir)
	if err != nil {
		return 2
	}
	if *temporary && !*worker {
		// Re-exec the same inode, including after an installation path update.
		// Configuration continues to travel only through inherited pipes.
		child := exec.Command("/proc/self/exe", "--data-dir", statePath, "--temporary-state", "--temporary-state-worker", "--supervisor-pid", strconv.Itoa(os.Getpid()))
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		return superviseTemporaryState(statePath, os.Stdin, signals, child)
	}
	if err := libbox.Setup(&libbox.SetupOptions{
		BasePath: statePath, WorkingPath: statePath, TempPath: filepath.Join(statePath, "tmp"),
	}); err != nil {
		return 2
	}
	s := &server{
		output: json.NewEncoder(os.Stdout), state: "disconnected",
		create: newCoreService, prepare: prepareStartConfig, permission: tunPermission,
	}
	s.serve(os.Stdin, signals)
	// A supervised worker never removes state itself: even a failed Close or
	// an uncooperative goroutine cannot access it after the supervisor's Wait.
	if *temporary && s.waitForWorkers() != nil {
		return 2
	}
	return 0
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

// Replies remain serialized. A retired worker can additionally notify the parent
// of terminal cleanup failure; its notification must never block worker exit or
// EOF/SIGTERM cleanup on the parent's output pipe.
func (s *server) emit(state string) {
	s.outputAccess.Lock()
	defer s.outputAccess.Unlock()
	_ = s.output.Encode(s.status(state))
}

func (s *server) status(state string) map[string]string {
	s.access.Lock()
	defer s.access.Unlock()
	if state == "" {
		state = s.state
	}
	status := map[string]string{"type": "status", "state": state}
	if s.cleanupFailed {
		status["state"], status["code"], status["message"] = "error", "cleanup_failed", cleanupFailureMessage
	}
	return status
}

func (s *server) setState(state string) {
	s.access.Lock()
	if !s.cleanupFailed {
		s.state = state
	}
	s.access.Unlock()
	s.emit(state)
}

func (s *server) reply(r request, result any) {
	s.outputAccess.Lock()
	defer s.outputAccess.Unlock()
	if r.Method == "start" || r.Method == "stop" || r.Method == "initialize" {
		s.access.Lock()
		failed := s.cleanupFailed
		s.access.Unlock()
		if failed {
			_ = s.output.Encode(map[string]any{"id": r.ID, "error": map[string]string{"code": "cleanup_failed", "message": cleanupFailureMessage}})
			return
		}
	}
	_ = s.output.Encode(map[string]any{"id": r.ID, "result": result})
}

func (s *server) fail(r request, code, message string) {
	s.outputAccess.Lock()
	defer s.outputAccess.Unlock()
	s.access.Lock()
	if s.cleanupFailed {
		code, message = "cleanup_failed", cleanupFailureMessage
	}
	s.access.Unlock()
	_ = s.output.Encode(map[string]any{"id": r.ID, "error": map[string]string{"code": code, "message": message}})
}

// closeService only relinquishes a successfully closed current instance. A
// failure, including one owned by a retired startup, poisons the whole helper:
// another connection cannot safely coexist with resources it failed to release.
func (s *server) closeService(instance *ownedService) {
	err := instance.Close()
	s.access.Lock()
	firstFailure := err != nil && !s.cleanupFailed
	if firstFailure {
		s.cleanupFailed, s.state = true, "error"
		if op := s.startup; op != nil {
			op.cancel()
			if !op.completed {
				op.completed = true
				close(op.done)
			}
		}
	} else if err == nil && s.service == instance {
		s.service = nil
	}
	s.access.Unlock()
	if firstFailure {
		// The failure may arrive after the cancelled RPC and stop have already
		// completed. Notify Dart even with no request pending so it can retire
		// the helper, without making cleanup depend on a writable output pipe.
		go s.emit("error")
	}
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
	if instance != nil {
		// Retain ownership until Close succeeds: simultaneous EOF/SIGTERM
		// cleanup must observe the same closure and its cached failure.
		s.closeService(instance)
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
	s.access.Lock()
	defer s.access.Unlock()
	if s.cleanupFailed {
		return errCleanupFailed
	}
	return nil
}

// waitForWorkers is for final shutdown after serve has cancelled its parent and
// closed the active service. Unlike stop's bounded wait, this includes retired
// workers, so their state directory cannot be removed while they still use it.
// Failed cleanup leaves resource lifetime uncertain even after all workers exit.
const finalWorkerDrainTimeout = 2 * time.Second

func (s *server) waitForWorkers() error {
	return s.waitForWorkersWithin(finalWorkerDrainTimeout)
}

func (s *server) waitForWorkersWithin(timeout time.Duration) error {
	s.access.Lock()
	s.shuttingDown = true
	s.access.Unlock()
	drained := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(drained)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		return context.DeadlineExceeded
	}
	s.access.Lock()
	defer s.access.Unlock()
	if s.cleanupFailed {
		return errCleanupFailed
	}
	return nil
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
	if parent.Err() != nil || s.shuttingDown {
		s.access.Unlock()
		return nil
	}
	if s.cleanupFailed {
		s.access.Unlock()
		s.fail(r, "cleanup_failed", cleanupFailureMessage)
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
	// Admission and shutdown's Wait barrier use the same lock. Add happens
	// before the worker can be launched or retired by a concurrent stop.
	s.workers.Add(1)
	s.access.Unlock()
	return op
}

func (s *server) currentStartLocked(op *startOperation) bool {
	return !s.cleanupFailed && s.startup == op && !op.completed && op.request.generation == s.stopGeneration && op.ctx.Err() == nil
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
	if s.cleanupFailed || s.startup != op || op.completed || op.request.generation != s.stopGeneration || op.parent.Err() != nil {
		return false
	}
	if err == nil {
		err = op.ctx.Err()
	}
	op.completed, op.succeeded = true, err == nil
	s.startup = nil
	if err != nil {
		// closeService has already released successful cleanup by identity, or
		// latched a terminal failure without discarding the failed owner.
		s.state = "error"
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

func (s *server) finishWorker(op *startOperation) {
	close(op.finished)
	s.workers.Done()
}

func (s *server) runStart(op *startOperation, config startConfig) {
	defer s.finishWorker(op)
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
			s.closeService(owned)
		}
	}
	if !s.finishStart(op, err) && owned != nil {
		// This instance belongs only to op, even if a newer service is active.
		s.closeService(owned)
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
		if !s.cleanupFailed {
			s.initialized = true
		}
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
		s.reply(r, s.status(""))
	case "start":
		s.access.Lock()
		failed, hasService := s.cleanupFailed, s.service != nil || s.startup != nil
		s.access.Unlock()
		if failed {
			s.fail(r, "cleanup_failed", cleanupFailureMessage)
			return
		}
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
			// Launch before pipe I/O: final shutdown must be able to drain
			// every admitted worker even if the parent stops reading replies.
			go s.runStart(op, config)
			s.emit("connecting")
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
			s.fail(r, "cleanup_failed", cleanupFailureMessage)
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
