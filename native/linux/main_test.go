// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	libbox "github.com/sagernet/sing-box/experimental/libbox"
)

type fakeService struct {
	startErr error
	start    func() error
	onClose  func()
	closed   atomic.Bool
	once     sync.Once
}

func (s *fakeService) Start() error {
	if s.start != nil {
		return s.start()
	}
	return s.startErr
}
func (s *fakeService) Close() error {
	s.once.Do(func() {
		s.closed.Store(true)
		if s.onClose != nil {
			s.onClose()
		}
	})
	return nil
}

func testServer(output io.Writer, service *fakeService) *server {
	return &server{
		output: json.NewEncoder(output), state: "disconnected", initialized: true,
		create:     func(context.Context, *libbox.DesktopConfig) (coreService, error) { return service, nil },
		prepare:    func(string) (startConfig, error) { return startConfig{}, nil },
		permission: func() bool { return true },
	}
}

func TestStartFailureCleansUpAndRedactsDiagnostics(t *testing.T) {
	var output bytes.Buffer
	service := &fakeService{startErr: errors.New("secret-auth-key")}
	s := testServer(&output, service)
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: `{}`})
	if !service.closed.Load() || s.service != nil || s.state != "error" {
		t.Fatal("failed startup must close resources without reporting connected")
	}
	if strings.Contains(output.String(), "secret-auth-key") || strings.Contains(output.String(), `"state":"connected"`) {
		t.Fatal("raw errors or false success escaped onto the protocol")
	}
}

func TestLifecycleAndDuplicateStart(t *testing.T) {
	var output bytes.Buffer
	service := &fakeService{}
	s := testServer(&output, service)
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: `{}`})
	if s.state != "connected" || s.service == nil {
		t.Fatal("successful core start must report connected")
	}
	s.handle(context.Background(), request{ID: 2, Method: "start", Config: `{}`})
	if !strings.Contains(output.String(), "already_started") || service.closed.Load() {
		t.Fatal("duplicate start must preserve the active service")
	}
	s.handle(context.Background(), request{ID: 3, Method: "stop"})
	if !service.closed.Load() || s.service != nil || s.state != "disconnected" {
		t.Fatal("stop must release the active service")
	}
}

func TestPermissionDenialDoesNotCreateService(t *testing.T) {
	var output bytes.Buffer
	service := &fakeService{}
	s := testServer(&output, service)
	// Protocol lifecycle tests must not depend on the host's optional registry.
	s.prepare = func(string) (startConfig, error) { return startConfig{hasTun: true}, nil }
	s.permission = func() bool { return false }
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: `{}`})
	if s.service != nil || s.state != "disconnected" || !strings.Contains(output.String(), "permission_denied") {
		t.Fatal("unprivileged TUN start must fail before creating an engine")
	}
}

func TestFailedFactoryReturnsNilInterfaceAndNormalError(t *testing.T) {
	// An empty inline rule set parses for every registry, then fails construction.
	const content = `{"route":{"final":"missing","rule_set":[{"type":"inline","tag":"empty","rules":[]}]}}`
	config, err := libbox.ParseDesktopConfig(content)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := newCoreService(context.Background(), config)
	if err == nil || instance != nil {
		t.Fatal("failed concrete construction must preserve a nil service interface")
	}
	var output bytes.Buffer
	s := testServer(&output, &fakeService{})
	s.prepare, s.create = prepareStartConfig, newCoreService
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: content})
	if !strings.Contains(output.String(), "start_failed") || s.service != nil {
		t.Fatal("construction failure must be a recoverable response")
	}
}

func TestParentEOFClosesService(t *testing.T) {
	var output bytes.Buffer
	service := &fakeService{}
	s := testServer(&output, service)
	s.service = service
	s.serve(strings.NewReader(""), make(chan os.Signal))
	if !service.closed.Load() || s.service != nil {
		t.Fatal("parent pipe closure must clean up the TUN and routes")
	}
}

type lockedOutput struct {
	sync.Mutex
	bytes.Buffer
}

func (b *lockedOutput) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}
func (b *lockedOutput) text() string {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.String()
}

func await(t *testing.T, ready <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func TestShutdownInterruptsInFlightStartup(t *testing.T) {
	for _, signal := range []bool{false, true} {
		t.Run(fmt.Sprintf("signal=%v", signal), func(t *testing.T) {
			var output lockedOutput
			started, release := make(chan struct{}), make(chan struct{})
			// Model an upstream download that ignores context cancellation. Close
			// must still run and serve must return without waiting for Start.
			service := &fakeService{start: func() error { close(started); <-release; return nil }}
			s := testServer(&output, service)
			input, writer := io.Pipe()
			defer input.Close()
			defer writer.Close()
			defer close(release)
			signals := make(chan os.Signal, 1)
			stopped := make(chan struct{})
			go func() { s.serve(input, signals); close(stopped) }()
			_, _ = fmt.Fprintln(writer, `{"id":1,"method":"start","config":"{}"}`)
			await(t, started, "startup did not begin")
			// A queued request must not prevent the reader from detecting EOF.
			_, _ = fmt.Fprintln(writer, `{"id":2,"method":"getStatus"}`)
			if signal {
				signals <- syscall.SIGTERM
			} else {
				_ = writer.Close()
			}
			await(t, stopped, "parent shutdown blocked behind engine startup")
			if !service.closed.Load() {
				t.Fatal("shutdown did not clean the in-flight engine")
			}
		})
	}
}

func TestStopCancelsStartupAndReturnsSerializedReply(t *testing.T) {
	var output lockedOutput
	started := make(chan struct{})
	service := &fakeService{}
	s := testServer(&output, service)
	s.create = func(ctx context.Context, _ *libbox.DesktopConfig) (coreService, error) {
		service.start = func() error { close(started); <-ctx.Done(); return ctx.Err() }
		return service, nil
	}
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	stopped := make(chan struct{})
	go func() { s.serve(input, make(chan os.Signal)); close(stopped) }()
	_, _ = fmt.Fprintln(writer, `{"id":1,"method":"start","config":"{}"}`)
	await(t, started, "startup did not begin")
	_, _ = fmt.Fprintln(writer, `{"id":2,"method":"stop"}`)
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(output.text(), `"id":2,"result":null`) {
		if time.Now().After(deadline) {
			t.Fatal("stop remained blocked behind startup")
		}
		time.Sleep(time.Millisecond)
	}
	if !service.closed.Load() || !strings.Contains(output.text(), `"state":"disconnected"`) {
		t.Fatal("stop did not release the interrupted engine")
	}
	_ = writer.Close()
	await(t, stopped, "helper did not exit after EOF")
}

// This controlled, credential-free fixture runs only in a disposable Linux
// network namespace. It keeps detailed engine errors out of the production IPC.
func TestRealLinuxTun(t *testing.T) {
	if os.Getenv("SINGBOX_TEST_TUN") != "1" {
		t.Skip("requires an isolated network namespace with TUN permissions")
	}
	data := t.TempDir()
	if err := libbox.Setup(&libbox.SetupOptions{BasePath: data, WorkingPath: data, TempPath: data}); err != nil {
		t.Fatal(err)
	}
	instance, err := libbox.NewDesktopService(context.Background(), `{"dns":{"servers":[{"type":"udp","tag":"dns-direct","server":"1.1.1.1"}],"final":"dns-direct"},"inbounds":[{"type":"tun","interface_name":"sbf-gotest","address":["172.29.199.1/30"],"auto_route":true,"stack":"system"}],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"auto_detect_interface":true,"final":"direct"}}`)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if err = instance.Start(); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeRequiredAndConfigRedaction(t *testing.T) {
	var output bytes.Buffer
	s := testServer(&output, &fakeService{})
	s.initialized = false
	s.handle(context.Background(), request{ID: 1, Method: "start", Config: `{}`})
	if !strings.Contains(output.String(), "not_initialized") {
		t.Fatal("start must require initialization")
	}
	s.handle(context.Background(), request{ID: 2, Method: "initialize"})
	s.handle(context.Background(), request{ID: 3, Method: "checkConfig", Config: `{"outbounds":[{"type":"secret-auth-key"}]}`})
	if !strings.Contains(output.String(), "invalid_config") || strings.Contains(output.String(), "secret-auth-key") {
		t.Fatal("config errors must be sanitized")
	}
}
