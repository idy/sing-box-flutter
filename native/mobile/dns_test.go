// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type cancelFunc func() error

func (f cancelFunc) Invoke() error { return f() }

func TestDNSCompletionReleasesCancelCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := &ExchangeContext{context: ctx}
	var called atomic.Bool
	response.OnCancel(cancelFunc(func() error { called.Store(true); return nil }))
	response.Success("192.0.2.1\n2001:db8::1")
	response.finish()
	if len(response.cancellations) != 0 {
		t.Fatal("completed query retained cancellation callback")
	}
	cancel()
	response.Success("invalid-late-value")
	if response.error != nil || len(response.addresses) != 2 || called.Load() {
		t.Fatal("completed query was modified")
	}
}

// deferredCancelContext exposes cancellation before dispatching AfterFunc, as
// happens while a canceled parent is still visiting its other children.
type deferredCancelContext struct {
	context.Context
	done chan struct{}
}

func (c *deferredCancelContext) Done() <-chan struct{} { return c.done }
func (c *deferredCancelContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}
func (c *deferredCancelContext) AfterFunc(func()) func() bool {
	return func() bool { return true }
}

func TestDNSFinishDeliversCanceledRegistrationOutsideMutex(t *testing.T) {
	ctx := &deferredCancelContext{Context: context.Background(), done: make(chan struct{})}
	response := &ExchangeContext{context: ctx}
	var calls atomic.Int32
	response.OnCancel(cancelFunc(func() error {
		calls.Add(1)
		// Host callbacks may synchronously reenter the response.
		response.Success("192.0.2.1")
		return nil
	}))
	close(ctx.done)
	finished := make(chan struct{})
	go func() {
		response.finish()
		response.finish()
		close(finished)
	}()
	receive(t, finished)
	if calls.Load() != 1 || len(response.cancellations) != 0 || len(response.addresses) != 0 {
		t.Fatal("finish dropped or duplicated a canceled registration")
	}
}

func TestDNSFinishDoesNotDuplicateOrWaitForRunningCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := &ExchangeContext{context: ctx}
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	response.OnCancel(cancelFunc(func() error {
		calls.Add(1)
		close(started)
		<-release
		close(returned)
		return nil
	}))
	cancel()
	receive(t, started)
	finished := make(chan struct{})
	go func() { response.finish(); close(finished) }()
	receive(t, finished)
	if calls.Load() != 1 {
		t.Fatal("already running callback was invoked twice")
	}
	// release is closed by the defer after finish has proved it does not wait.
	t.Cleanup(func() { receive(t, returned) })
}

func TestDNSCancelFinishRaceUnblocksEveryRegistration(t *testing.T) {
	for range 1000 {
		ctx, cancel := context.WithCancel(context.Background())
		response := &ExchangeContext{context: ctx}
		var calls atomic.Int32
		returned := make(chan struct{}, 2)
		response.OnCancel(cancelFunc(func() error { calls.Add(1); returned <- struct{}{}; return nil }))
		go cancel()
		<-ctx.Done()
		response.finish()
		select {
		case <-returned:
		case <-time.After(3 * time.Second):
			t.Fatal("canceled platform DNS remained blocked")
		}
		if calls.Load() != 1 {
			t.Fatal("cancellation was invoked more than once")
		}
	}
}

func TestDNSCancellationRegisteredAfterCompletionStillUnblocksHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	response := &ExchangeContext{context: ctx}
	cancel()
	response.finish()
	called := false
	response.OnCancel(cancelFunc(func() error { called = true; return nil }))
	if !called {
		t.Fatal("late cancellation registration could strand platform DNS")
	}
}
