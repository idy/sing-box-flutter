// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	check  func(context.Context, string, string) error
	create func(context.Context, string, string) (core, error)
}

func (fakeBackend) Version() string { return "test-core" }
func (b fakeBackend) Check(ctx context.Context, config, path string) error {
	if b.check != nil {
		return b.check(ctx, config, path)
	}
	return nil
}
func (b fakeBackend) New(ctx context.Context, config, path string) (core, error) {
	if b.create != nil {
		return b.create(ctx, config, path)
	}
	return &fakeCore{}, nil
}

type fakeCore struct {
	start  func() error
	close  func() error
	closed atomic.Int32
}

func (c *fakeCore) Start() error {
	if c.start != nil {
		return c.start()
	}
	return nil
}
func (c *fakeCore) Close() error {
	c.closed.Add(1)
	if c.close != nil {
		return c.close()
	}
	return nil
}

type wireMessage struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *protocolError  `json:"error"`
	Event  *status         `json:"event"`
}
type captureWriter struct {
	messages chan wireMessage
	fail     atomic.Bool
	mu       sync.Mutex
	text     strings.Builder
}

func (w *captureWriter) Write(data []byte) (int, error) {
	if w.fail.Load() {
		return 0, io.ErrClosedPipe
	}
	var message wireMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return 0, err
	}
	w.mu.Lock()
	w.text.Write(data)
	w.mu.Unlock()
	w.messages <- message
	return len(data), nil
}

type protocolHarness struct {
	t      *testing.T
	server *server
	input  *io.PipeWriter
	output *captureWriter
	done   chan struct{}
}

func newHarness(t *testing.T, b backend) *protocolHarness {
	t.Helper()
	input, writer := io.Pipe()
	output := &captureWriter{messages: make(chan wireMessage, 100)}
	s := newServer(output, b)
	s.storage = func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil }
	s.admin = func() bool { return true }
	h := &protocolHarness{t: t, server: s, input: writer, output: output, done: make(chan struct{})}
	go func() { defer close(h.done); s.run(context.Background(), input) }()
	t.Cleanup(func() { writer.Close(); await(t, h.done) })
	return h
}
func await(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("helper operation timed out")
	}
}
func (h *protocolHarness) send(id int, method string, arguments map[string]any) {
	h.t.Helper()
	data, err := json.Marshal(map[string]any{"id": id, "method": method, "arguments": arguments})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err = h.input.Write(append(data, '\n')); err != nil {
		h.t.Fatal(err)
	}
}
func (h *protocolHarness) next() wireMessage {
	h.t.Helper()
	select {
	case result := <-h.output.messages:
		return result
	case <-time.After(3 * time.Second):
		h.t.Fatal("missing protocol response")
		return wireMessage{}
	}
}
func (h *protocolHarness) response(id int, errorCode string) wireMessage {
	h.t.Helper()
	result := h.next()
	if result.ID == nil || *result.ID != id || result.Event != nil {
		h.t.Fatalf("wanted response %d: %+v", id, result)
	}
	if errorCode == "" {
		if result.Error != nil || result.Result == nil {
			h.t.Fatalf("wanted result: %+v", result)
		}
	} else if result.Error == nil || result.Error.Code != errorCode {
		h.t.Fatalf("wanted error %s: %+v", errorCode, result)
	}
	return result
}
func (h *protocolHarness) event(state string) {
	h.t.Helper()
	result := h.next()
	if result.Event == nil || result.Event.Type != "status" || result.Event.State != state || result.ID != nil {
		h.t.Fatalf("wanted %s event: %+v", state, result)
	}
}
func (h *protocolHarness) initialize() {
	h.send(1, "initialize", map[string]any{"workingPath": "test-private-app-path"})
	h.response(1, "")
}

func TestProtocolInitializationAndArguments(t *testing.T) {
	h := newHarness(t, fakeBackend{})
	h.send(1, "getVersion", nil)
	if got := string(h.response(1, "").Result); got != `"test-core"` {
		t.Fatal(got)
	}
	for _, method := range []string{"checkConfig", "requestPermission", "start", "stop", "getStatus"} {
		h.send(2, method, nil)
		h.response(2, "not_initialized")
	}
	h.send(3, "initialize", nil)
	h.response(3, "invalid_arguments")
	h.initialize()
	h.initialize() // Same app initialization is idempotent.
	h.send(4, "initialize", map[string]any{"workingPath": "another-app"})
	h.response(4, "already_initialized")
	h.send(5, "getStatus", nil)
	var initial status
	if json.Unmarshal(h.response(5, "").Result, &initial) != nil || initial.State != "disconnected" {
		t.Fatal(initial)
	}
	h.send(6, "checkConfig", map[string]any{"config": 42})
	h.response(6, "invalid_arguments")
	h.send(7, "not-a-method", nil)
	h.response(7, "unsupported_method")
	for _, raw := range []string{`{"method":"start"}`, `{"id":1.5,"method":"start"}`, `{"id":2,"method":"start","arguments":[]}`, `null`, `secret-invalid-json`} {
		if _, err := io.WriteString(h.input, raw+"\n"); err != nil {
			t.Fatal(err)
		}
		h.response(0, "invalid_request")
	}
}

func TestProtocolConnectedStopAndEOF(t *testing.T) {
	instance := &fakeCore{}
	h := newHarness(t, fakeBackend{create: func(_ context.Context, config, path string) (core, error) {
		if config != "private-config" || path != "test-private-app-path" {
			t.Error("incorrect operation inputs")
		}
		return instance, nil
	}})
	h.initialize()
	h.send(2, "start", map[string]any{"config": "private-config", "name": "private-name"})
	h.response(2, "")
	h.event("connecting")
	h.event("connected")
	h.send(3, "start", map[string]any{"config": "second-config"})
	h.response(3, "busy")
	h.send(4, "stop", nil)
	h.event("disconnecting")
	h.event("disconnected")
	h.response(4, "")
	if instance.closed.Load() != 1 {
		t.Fatal("stop did not close exactly once")
	}
	// Start another core and lose the owning app's input pipe.
	second := &fakeCore{}
	h.server.backend = fakeBackend{create: func(context.Context, string, string) (core, error) { return second, nil }}
	h.send(5, "start", map[string]any{"config": "new-config"})
	h.response(5, "")
	h.event("connecting")
	h.event("connected")
	h.input.Close()
	await(t, h.done)
	if second.closed.Load() != 1 {
		t.Fatal("EOF did not dispose connected core")
	}
}

func TestProtocolCancelSlowStartKeepsStatusLiveAndOrdersCleanup(t *testing.T) {
	entered, release, cancelled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	instance := &fakeCore{}
	var creations atomic.Int32
	h := newHarness(t, fakeBackend{create: func(ctx context.Context, _, _ string) (core, error) {
		creations.Add(1)
		instance.start = func() error {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-release
			return errors.New("secret-from-start")
		}
		return instance, nil
	}})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h.initialize()
	h.send(2, "start", map[string]any{"config": "secret-config"})
	h.response(2, "")
	h.event("connecting")
	await(t, entered)
	h.send(3, "getStatus", nil)
	if !strings.Contains(string(h.response(3, "").Result), `"state":"connecting"`) {
		t.Fatal("status is not live during startup")
	}
	h.send(4, "stop", nil)
	h.event("disconnecting")
	await(t, cancelled)
	h.send(5, "getStatus", nil)
	if !strings.Contains(string(h.response(5, "").Result), `"state":"disconnecting"`) {
		t.Fatal("cancel state lost")
	}
	h.send(6, "start", map[string]any{"config": "must-not-leak"})
	h.response(6, "busy")
	h.send(7, "stop", nil)
	// Pipe Write confirms scanner delivery, not that the server handled stop.
	// A later response is a barrier before allowing startup to finish cleanup.
	h.send(8, "getStatus", nil)
	if !strings.Contains(string(h.response(8, "").Result), `"state":"disconnecting"`) {
		t.Fatal("second stop did not remain pending during startup")
	}
	if instance.closed.Load() != 0 {
		t.Fatal("Close raced Start")
	}
	close(release)
	h.event("disconnected")
	h.response(4, "")
	h.response(7, "")
	if instance.closed.Load() != 1 || creations.Load() != 1 {
		t.Fatal("concurrent configs or duplicate cleanup")
	}
}

func TestProtocolEOFWhileConstructorIsPending(t *testing.T) {
	entered := make(chan struct{})
	instance := &fakeCore{}
	h := newHarness(t, fakeBackend{create: func(ctx context.Context, _, _ string) (core, error) {
		close(entered)
		<-ctx.Done()
		return instance, errors.New("private-constructor-details")
	}})
	h.initialize()
	h.send(2, "start", map[string]any{"config": "secret"})
	h.response(2, "")
	h.event("connecting")
	await(t, entered)
	h.input.Close()
	await(t, h.done)
	if instance.closed.Load() != 1 {
		t.Fatal("EOF retained partially constructed core")
	}
}

func TestProtocolSanitizesEveryBackendFailure(t *testing.T) {
	for _, stage := range []string{"check", "constructor", "start", "close"} {
		t.Run(stage, func(t *testing.T) {
			secret := "https://auth.example/secret-token?key=private"
			instance := &fakeCore{}
			b := fakeBackend{}
			b.check = func(context.Context, string, string) error { return errors.New(secret) }
			b.create = func(context.Context, string, string) (core, error) {
				if stage == "constructor" {
					return instance, errors.New(secret)
				}
				if stage == "start" {
					instance.start = func() error { return errors.New(secret) }
				}
				if stage == "close" {
					instance.close = func() error { return errors.New(secret) }
				}
				return instance, nil
			}
			h := newHarness(t, b)
			h.initialize()
			h.send(2, "checkConfig", map[string]any{"config": secret})
			h.response(2, "invalid_config")
			if stage != "check" {
				h.send(3, "start", map[string]any{"config": secret})
				h.response(3, "")
				h.event("connecting")
				if stage == "close" {
					h.event("connected")
					h.send(4, "stop", nil)
					h.event("disconnecting")
					h.event("error")
					h.response(4, "cleanup_failed")
				} else {
					h.event("error")
				}
				if instance.closed.Load() != 1 {
					t.Fatal("failed backend retained core")
				}
			}
			h.output.mu.Lock()
			text := h.output.text.String()
			h.output.mu.Unlock()
			if strings.Contains(text, secret) || strings.Contains(text, "secret-token") {
				t.Fatalf("failure leaked credentials: %s", text)
			}
		})
	}
}

func TestProtocolSlowCheckKeepsStopAndEOFLive(t *testing.T) {
	entered, cleaned := make(chan struct{}), make(chan struct{})
	h := newHarness(t, fakeBackend{check: func(ctx context.Context, _, _ string) error {
		close(entered)
		<-ctx.Done()
		close(cleaned)
		return ctx.Err()
	}})
	h.initialize()
	h.send(2, "checkConfig", map[string]any{"config": "slow-check"})
	await(t, entered)
	h.send(3, "getStatus", nil)
	h.response(3, "")
	h.send(4, "stop", nil)
	h.event("disconnected")
	h.response(4, "")
	h.input.Close()
	await(t, h.done)
	await(t, cleaned)
}

func TestProtocolPermissionAndOutputPipeFailure(t *testing.T) {
	instance := &fakeCore{}
	h := newHarness(t, fakeBackend{create: func(context.Context, string, string) (core, error) { return instance, nil }})
	h.server.admin = func() bool { return false }
	h.initialize()
	h.send(2, "requestPermission", nil)
	if string(h.response(2, "").Result) != "false" {
		t.Fatal("permission was simulated")
	}
	h.send(3, "start", map[string]any{"config": "config"})
	h.response(3, "permission_denied")
	h.server.admin = func() bool { return true }
	h.send(4, "start", map[string]any{"config": "config"})
	h.response(4, "")
	h.event("connecting")
	h.event("connected")
	h.output.fail.Store(true)
	h.send(5, "getStatus", nil)
	await(t, h.done)
	if instance.closed.Load() != 1 {
		t.Fatal("output pipe failure retained core")
	}
}
