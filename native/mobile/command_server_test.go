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
	close  func() error
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
func (e *fakeMobileEngine) Close() error {
	e.closed.Add(1)
	if e.close != nil {
		return e.close()
	}
	return nil
}
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

func requirePermanentCloseFailure(t *testing.T, server *CommandServer) {
	t.Helper()
	for range 3 {
		err := server.CloseWithError()
		if err == nil || err.Error() != "could not stop mobile service cleanly; restart the process" {
			t.Fatalf("missing sanitized permanent close failure: %v", err)
		}
		server.Close() // The compatibility wrapper must not erase the result.
	}
	if server.Start() == nil || server.StartOrReloadService(lifecycleTestConfig, nil) == nil {
		t.Fatal("failed cleanup allowed another service")
	}
	if _, err := server.PrepareService(lifecycleTestConfig, nil); err == nil {
		t.Fatal("failed cleanup allowed preparation")
	}
	if err := server.CloseService(); err == nil || err.Error() != "could not stop mobile service cleanly; restart the process" {
		t.Fatal("service close erased permanent failure")
	}
}

func TestPermanentCloseKeepsActiveAndPreparedResults(t *testing.T) {
	for _, phase := range []string{"active", "prepared"} {
		for _, fails := range []bool{false, true} {
			t.Run(phase+map[bool]string{false: "/success", true: "/failure"}[fails], func(t *testing.T) {
				engine := &fakeMobileEngine{close: func() error {
					if fails {
						return errors.New("private-close-token")
					}
					return nil
				}}
				server := &CommandServer{standalone: true, newEngine: func(context.Context, PlatformInterface, option.Options) (mobileEngine, error) {
					return engine, nil
				}}
				if phase == "active" {
					if err := server.StartOrReloadService(lifecycleTestConfig, nil); err != nil {
						t.Fatal(err)
					}
				} else if _, err := server.PrepareService(lifecycleTestConfig, nil); err != nil {
					t.Fatal(err)
				}
				server.Close() // Legacy close must still preserve an inspectable result.
				if fails {
					requirePermanentCloseFailure(t, server)
				} else {
					for range 3 {
						if err := server.CloseWithError(); err != nil {
							t.Fatal(err)
						}
					}
				}
				if engine.closed.Load() != 1 {
					t.Fatal("permanent close retried cleanup")
				}
			})
		}
	}
}

func TestPermanentCloseKeepsCancelledStartupCleanupFailure(t *testing.T) {
	for _, phase := range []string{"mobile", "prepare", "activate"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan struct{})
			engine := &fakeMobileEngine{close: func() error { return errors.New("private-close-token") }}
			server := &CommandServer{standalone: phase != "mobile", newEngine: func(ctx context.Context, _ PlatformInterface, _ option.Options) (mobileEngine, error) {
				block := func() error { close(entered); <-ctx.Done(); return errors.New("private-start-token") }
				if phase == "prepare" {
					return engine, block()
				}
				engine.start = block
				return engine, nil
			}}
			started := make(chan error, 1)
			go func() {
				if phase == "mobile" {
					started <- server.StartOrReloadService(lifecycleTestConfig, nil)
					return
				}
				_, err := server.PrepareService(lifecycleTestConfig, nil)
				if err == nil {
					err = server.ActivateService()
				}
				started <- err
			}()
			receive(t, entered)
			closed := make(chan error, 1)
			go func() { closed <- server.CloseWithError() }()
			if err := receive(t, closed); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("cancelled startup lost cleanup error: %v", err)
			}
			if err := receive(t, started); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("cancelled startup did not fail safely: %v", err)
			}
			requirePermanentCloseFailure(t, server)
			if engine.closed.Load() != 1 {
				t.Fatal("cancelled engine cleanup was retried")
			}
		})
	}
}

func TestConcurrentPermanentCloseWaitsAndReturnsFailure(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	engine := &fakeMobileEngine{close: func() error {
		close(entered)
		<-release
		return errors.New("private-close-token")
	}}
	server := &CommandServer{engine: engine, cancel: cancel}
	results := make(chan error, 8)
	for range cap(results) {
		go func() { results <- server.CloseWithError() }()
	}
	receive(t, entered)
	if ctx.Err() == nil || server.Start() == nil {
		t.Fatal("permanent close did not cancel/close before blocking cleanup")
	}
	select {
	case <-results:
		t.Fatal("concurrent close returned before cleanup completed")
	default:
	}
	close(release)
	for range cap(results) {
		if err := receive(t, results); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("concurrent close lost sanitized failure: %v", err)
		}
	}
	requirePermanentCloseFailure(t, server)
	if engine.closed.Load() != 1 {
		t.Fatal("concurrent close retried engine cleanup")
	}
}

func TestServiceCloseFailurePoisonsServerAndDrainsBothOwners(t *testing.T) {
	active := &fakeMobileEngine{close: func() error { return errors.New("private-token") }}
	prepared := &fakeMobileEngine{}
	server := &CommandServer{engine: active, prepared: &preparedStandaloneService{engine: prepared}}
	if err := server.CloseService(); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("service close lost cleanup failure: %v", err)
	}
	requirePermanentCloseFailure(t, server)
	if active.closed.Load() != 1 || prepared.closed.Load() != 1 {
		t.Fatal("failed cleanup skipped another owner or retried an engine")
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
