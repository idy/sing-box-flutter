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

	libbox "github.com/sagernet/sing-box/experimental/libbox"
	"golang.org/x/sys/unix"
)

type request struct {
	ID         int    `json:"id"`
	Method     string `json:"method"`
	Config     string `json:"config"`
	generation uint64
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

type server struct {
	output         *json.Encoder
	access         sync.Mutex
	service        coreService
	starting       bool
	cancelStart    context.CancelFunc
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
	if !filepath.IsAbs(*dataDir) {
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
			s.access.Unlock()
			if r.Method == "stop" {
				// Record cancellation before a just-dispatched start can publish.
				// Replies remain serialized after the interrupted start reply.
				s.interruptStartup()
			}
			pending = append(pending, r)
		}
	}
}

func (s *server) emit(state string) {
	s.state = state
	_ = s.output.Encode(map[string]any{"type": "status", "state": state})
}

func (s *server) reply(r request, result any) {
	_ = s.output.Encode(map[string]any{"id": r.ID, "result": result})
}

func (s *server) fail(r request, code, message string) {
	_ = s.output.Encode(map[string]any{"id": r.ID, "error": map[string]string{"code": code, "message": message}})
}

func (s *server) close() error {
	s.access.Lock()
	instance, cancel := s.service, s.cancelStart
	s.service, s.starting, s.cancelStart = nil, false, nil
	s.access.Unlock()
	if cancel != nil {
		cancel()
	}
	if instance != nil {
		return instance.Close()
	}
	return nil
}

func (s *server) interruptStartup() {
	s.access.Lock()
	s.stopGeneration++
	instance, cancel, starting := s.service, s.cancelStart, s.starting
	s.access.Unlock()
	if starting {
		if cancel != nil {
			cancel()
		}
		if instance != nil {
			go instance.Close()
		}
	}
}

func (s *server) beginStart(parent context.Context, generation uint64) (context.Context, bool) {
	s.access.Lock()
	defer s.access.Unlock()
	if parent.Err() != nil || generation != s.stopGeneration {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	s.starting, s.cancelStart = true, cancel
	return ctx, true
}

func (s *server) publish(ctx context.Context, instance coreService) bool {
	s.access.Lock()
	defer s.access.Unlock()
	if ctx.Err() != nil {
		return false
	}
	s.service, s.starting = instance, true
	return true
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
		s.initialized = true
		s.reply(r, nil)
		return
	}
	if !s.initialized {
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
		s.reply(r, map[string]string{"type": "status", "state": s.state})
	case "start":
		s.access.Lock()
		hasService := s.service != nil
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
		startContext, allowed := s.beginStart(ctx, r.generation)
		if !allowed {
			if ctx.Err() == nil {
				s.fail(r, "start_failed", "Linux service startup was stopped.")
			}
			return
		}
		s.emit("connecting")
		instance, err := s.create(startContext, config.native)
		published := false
		if err == nil && instance == nil {
			err = errors.New("missing Linux service")
		}
		if err == nil {
			published = s.publish(startContext, instance)
			if published {
				err = instance.Start()
			} else {
				err = context.Canceled
			}
		}
		if err != nil || startContext.Err() != nil {
			_ = s.close()
			if !published && instance != nil {
				_ = instance.Close()
			}
			if ctx.Err() != nil {
				return
			}
			s.emit("error")
			s.fail(r, "start_failed", "Could not start the Linux core. Check the configuration, TUN permissions and routing conflicts.")
			return
		}
		s.access.Lock()
		s.starting = false
		s.access.Unlock()
		s.emit("connected")
		s.reply(r, nil)
	case "stop":
		s.emit("disconnecting")
		if s.close() != nil {
			s.emit("error")
			s.fail(r, "stop_failed", "Could not fully close the Linux core.")
			return
		}
		s.emit("disconnected")
		s.reply(r, nil)
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
