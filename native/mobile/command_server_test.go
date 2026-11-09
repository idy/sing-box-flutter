// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/option"
)

// Fake engines never resolve the final tag. An explicit final keeps lifecycle
// tests independent of whether the compiled profile selects implicit direct.
const lifecycleTestConfig = `{"route":{"final":"fixture"}}`

type fakeMobileEngine struct {
	ctx    context.Context
	start  func() error
	closed atomic.Int32
	pauses atomic.Int32
	wakes  atomic.Int32
}

func (e *fakeMobileEngine) Start() error {
	if e.start != nil {
		return e.start()
	}
	return nil
}
func (e *fakeMobileEngine) Close() error { e.closed.Add(1); return nil }
func (e *fakeMobileEngine) Pause() {
	if e.closed.Load() != 0 {
		panic("pause after close")
	}
	e.pauses.Add(1)
}
func (e *fakeMobileEngine) Wake() {
	if e.closed.Load() != 0 {
		panic("wake after close")
	}
	e.wakes.Add(1)
}
func (e *fakeMobileEngine) NeedWIFIState() bool {
	if e.closed.Load() != 0 {
		panic("query after close")
	}
	return true
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case result := <-channel:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("mobile lifecycle did not complete")
		var zero T
		return zero
	}
}

func TestCloseCancelsStartupAndIsFinal(t *testing.T) {
	started := make(chan struct{})
	engine := &fakeMobileEngine{}
	server := &CommandServer{newEngine: func(ctx context.Context, _ PlatformInterface, _ option.Options) (mobileEngine, error) {
		engine.ctx = ctx
		engine.start = func() error { close(started); <-ctx.Done(); return errors.New("secret-auth-key") }
		return engine, nil
	}}
	result := make(chan error, 1)
	go func() { result <- server.StartOrReloadService(lifecycleTestConfig, nil) }()
	receive(t, started)
	closed := make(chan struct{})
	go func() { server.Close(); close(closed) }()
	receive(t, closed)
	if err := receive(t, result); err == nil || strings.Contains(err.Error(), "secret-auth-key") {
		t.Fatalf("unsanitized/missing failure: %v", err)
	}
	server.Close()
	if engine.closed.Load() != 1 {
		t.Fatal("partial engine was not closed exactly once")
	}
	if server.Start() == nil || server.StartOrReloadService(lifecycleTestConfig, nil) == nil || server.CloseService() == nil {
		t.Fatal("closed server accepted work")
	}
	server.Pause()
	server.Wake()
	if server.NeedWIFIState() || engine.pauses.Load() != 0 || engine.wakes.Load() != 0 {
		t.Fatal("closed server used its engine")
	}
}

func TestCloseServiceInterruptsStartupAndAllowsRestart(t *testing.T) {
	started := make(chan struct{})
	var calls int
	server := &CommandServer{newEngine: func(ctx context.Context, _ PlatformInterface, _ option.Options) (mobileEngine, error) {
		calls++
		engine := &fakeMobileEngine{ctx: ctx}
		if calls == 1 {
			engine.start = func() error { close(started); <-ctx.Done(); return ctx.Err() }
		}
		return engine, nil
	}}
	defer server.Close()
	result := make(chan error, 1)
	go func() { result <- server.StartOrReloadService(lifecycleTestConfig, nil) }()
	receive(t, started)
	stopped := make(chan error, 1)
	go func() { stopped <- server.CloseService() }()
	if err := receive(t, stopped); err != nil {
		t.Fatal(err)
	}
	if receive(t, result) == nil {
		t.Fatal("stopped startup succeeded")
	}
	if err := server.StartOrReloadService(lifecycleTestConfig, nil); err != nil {
		t.Fatal(err)
	}
	if !server.NeedWIFIState() {
		t.Fatal("replacement service is absent")
	}
}

func TestReloadClosesPreviousAndCleansFailedReplacement(t *testing.T) {
	first := &fakeMobileEngine{}
	second := &fakeMobileEngine{start: func() error { return errors.New("credential-bearing-url") }}
	var calls int
	server := &CommandServer{newEngine: func(context.Context, PlatformInterface, option.Options) (mobileEngine, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		if first.closed.Load() != 1 {
			t.Error("reload constructed replacement before closing previous service")
		}
		return second, nil
	}}
	defer server.Close()
	if err := server.StartOrReloadService(lifecycleTestConfig, nil); err != nil {
		t.Fatal(err)
	}
	if err := server.StartOrReloadService("{invalid-credential", nil); err == nil || strings.Contains(err.Error(), "credential") {
		t.Fatal("invalid config error was not sanitized")
	}
	if first.closed.Load() != 0 {
		t.Fatal("decode failure destroyed running service")
	}
	if err := server.StartOrReloadService(lifecycleTestConfig, nil); err == nil || strings.Contains(err.Error(), "credential") {
		t.Fatal("engine error was not sanitized")
	}
	if first.closed.Load() != 1 || second.closed.Load() != 1 {
		t.Fatal("reload leaked an engine")
	}
	if server.NeedWIFIState() {
		t.Fatal("failed replacement was published")
	}
}

func TestConstructorFailureClosesReturnedResources(t *testing.T) {
	engine := &fakeMobileEngine{}
	server := &CommandServer{newEngine: func(context.Context, PlatformInterface, option.Options) (mobileEngine, error) {
		return engine, errors.New("private-token")
	}}
	defer server.Close()
	if err := server.StartOrReloadService(lifecycleTestConfig, nil); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("missing sanitized constructor failure")
	}
	if engine.closed.Load() != 1 {
		t.Fatal("constructor resources leaked")
	}
}

func TestConcurrentLifecycleIsSerialized(t *testing.T) {
	var engines []*fakeMobileEngine
	server := &CommandServer{newEngine: func(context.Context, PlatformInterface, option.Options) (mobileEngine, error) {
		engine := &fakeMobileEngine{}
		engines = append(engines, engine)
		return engine, nil
	}}
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 8 {
				_ = server.StartOrReloadService(lifecycleTestConfig, nil)
				server.Pause()
				server.Wake()
				_ = server.NeedWIFIState()
				_ = server.CloseService()
			}
		}()
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	receive(t, done)
	server.Close()
	for _, engine := range engines {
		if engine.closed.Load() != 1 {
			t.Fatal("engine was leaked or closed twice")
		}
	}
}
