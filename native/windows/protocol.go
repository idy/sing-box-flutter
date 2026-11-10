// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

const maxRequestBytes = 8 << 20

type request struct {
	ID        *int                       `json:"id"`
	Method    string                     `json:"method"`
	Arguments map[string]json.RawMessage `json:"arguments"`
}

type protocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type status struct {
	Type    string `json:"type"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

type core interface {
	Start() error
	Close() error
}

type backend interface {
	Version() string
	Check(context.Context, string, string) error
	New(context.Context, string, string) (core, error)
}

type connection struct {
	cancel context.CancelFunc
	done   chan struct{}
	stops  []int
}

// The protocol reader never waits for Start or Close. A single connection worker
// owns both operations, so a cancelled startup must finish cleanup before another
// config can be accepted. In particular, Close is never raced against box.Start.
type server struct {
	backend  backend
	setup    func(string) error
	lock     func(string) (io.Closer, error)
	lease    io.Closer
	admin    func() bool
	output   *json.Encoder
	writeMu  sync.Mutex
	failed   chan struct{}
	mu       sync.Mutex
	working  string
	current  status
	active   *connection
	closed   bool
	lifetime context.Context
	cancel   context.CancelFunc
	checks   sync.WaitGroup
}

func newServer(output io.Writer, engine backend) *server {
	ctx, cancel := context.WithCancel(context.Background())
	return &server{
		lifetime: ctx, cancel: cancel,
		backend: engine, setup: initializeDirectories, lock: lockWorkingDirectory, admin: isElevated,
		output: json.NewEncoder(output), failed: make(chan struct{}),
		current: status{Type: "status", State: "disconnected"},
	}
}

func (s *server) send(value any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	select {
	case <-s.failed:
		return
	default:
	}
	if err := s.output.Encode(value); err != nil {
		close(s.failed)
	}
}

func (s *server) result(id int, value any) {
	s.send(struct {
		ID     int `json:"id"`
		Result any `json:"result"`
	}{id, value})
}

func (s *server) failure(id int, code, message string) {
	s.send(struct {
		ID    int           `json:"id"`
		Error protocolError `json:"error"`
	}{id, protocolError{code, message}})
}

// Call with mu held so status events have the same order as getStatus snapshots.
func (s *server) state(state, message string) {
	s.current = status{Type: "status", State: state, Message: message}
	s.send(struct {
		Event status `json:"event"`
	}{s.current})
}

func (s *server) run(ctx context.Context, input io.ReadCloser) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer input.Close()
	defer s.shutdown()
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), maxRequestBytes)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		// EOF, a broken input pipe, and an oversized frame all end ownership of
		// the core. Never echo a scanner/OS error into the protocol.
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.failed:
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			s.handle(line)
		}
	}
}

func argument(r request, name string) (string, bool) {
	var value string
	err := json.Unmarshal(r.Arguments[name], &value)
	return value, err == nil && value != ""
}

func (s *server) handle(line []byte) {
	var r request
	if json.Unmarshal(line, &r) != nil || r.ID == nil || r.Method == "" {
		s.failure(0, "invalid_request", "Invalid helper request")
		return
	}
	id := *r.ID
	if r.Method == "getVersion" {
		s.result(id, s.backend.Version())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if r.Method == "initialize" {
		path, ok := argument(r, "workingPath")
		if !ok {
			s.failure(id, "invalid_arguments", "An absolute application working path is required")
		} else if s.working != "" {
			if path == s.working {
				s.result(id, nil)
			} else {
				s.failure(id, "already_initialized", "The helper is already initialized for another application path")
			}
		} else if s.setup(path) != nil {
			s.failure(id, "initialize_failed", "Could not initialize private application directories")
		} else {
			lease, err := s.lock(path)
			if err != nil {
				s.failure(id, "initialize_failed", "Could not acquire exclusive application core ownership")
			} else {
				s.working, s.lease = path, lease
				s.result(id, nil)
			}
		}
		return
	}
	if s.working == "" {
		s.failure(id, "not_initialized", "Initialize the helper before using this method")
		return
	}
	switch r.Method {
	case "getStatus":
		s.result(id, s.current)
	case "requestPermission":
		s.result(id, s.admin())
	case "checkConfig":
		content, ok := argument(r, "config")
		if !ok {
			s.failure(id, "invalid_arguments", "A configuration is required")
		} else {
			working := s.working
			s.checks.Add(1)
			go func() {
				defer s.checks.Done()
				if s.backend.Check(s.lifetime, content, working) != nil {
					s.failure(id, "invalid_config", "Invalid Windows configuration")
				} else {
					s.result(id, nil)
				}
			}()
		}
	case "start":
		content, ok := argument(r, "config")
		if !ok {
			s.failure(id, "invalid_arguments", "A configuration is required")
			return
		}
		if s.active != nil {
			s.failure(id, "busy", "A connection is already active or changing state")
			return
		}
		if !s.admin() {
			s.failure(id, "permission_denied", "Run the application as administrator to start the tunnel")
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		conn := &connection{cancel: cancel, done: make(chan struct{})}
		s.active = conn
		s.result(id, nil)
		s.state("connecting", "")
		go s.connect(ctx, conn, content, s.working)
	case "stop":
		if s.active == nil {
			s.state("disconnected", "")
			s.result(id, nil)
			return
		}
		conn := s.active
		conn.stops = append(conn.stops, id)
		if s.current.State != "disconnecting" {
			s.state("disconnecting", "")
			conn.cancel()
		}
	default:
		s.failure(id, "unsupported_method", "Unsupported helper method")
	}
}

func (s *server) connect(ctx context.Context, conn *connection, content, working string) {
	instance, err := s.backend.New(ctx, content, working)
	content = "" // Retain configs only in this operation, never in status or errors.
	if err == nil && ctx.Err() == nil {
		if instance == nil {
			err = errors.New("missing core")
		} else {
			err = instance.Start()
		}
	}
	s.mu.Lock()
	if err == nil && ctx.Err() == nil {
		s.state("connected", "")
		s.mu.Unlock()
		<-ctx.Done()
	} else {
		s.mu.Unlock()
	}
	// Always dispose a partial constructor or failed Start, including on EOF.
	var closeErr error
	if instance != nil {
		closeErr = instance.Close()
	}
	cancelled := ctx.Err() != nil
	conn.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !cancelled && err != nil {
		s.state("error", "Could not start the Windows tunnel")
	} else if closeErr != nil {
		s.state("error", "Could not completely stop the Windows tunnel")
	} else {
		s.state("disconnected", "")
	}
	for _, id := range conn.stops {
		if closeErr != nil {
			s.failure(id, "stop_failed", "Could not completely stop the Windows tunnel")
		} else {
			s.result(id, nil)
		}
	}
	s.active = nil
	close(conn.done)
}

func (s *server) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	conn := s.active
	if conn != nil {
		conn.cancel()
	}
	s.mu.Unlock()
	if conn != nil {
		<-conn.done
	}
	s.checks.Wait()
	if s.lease != nil {
		_ = s.lease.Close()
	}
}
