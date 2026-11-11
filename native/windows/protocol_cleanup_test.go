// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProtocolCleanupFailureRetiresWithoutReusingCore(t *testing.T) {
	for _, stage := range []string{"stop", "start", "constructor"} {
		t.Run(stage, func(t *testing.T) {
			const secret = "synthetic-private-backend-diagnostic"
			cleanupEntered, cleanupRelease := make(chan struct{}), make(chan struct{})
			instance := &fakeCore{close: func() error {
				close(cleanupEntered)
				<-cleanupRelease
				return errors.New(secret)
			}}
			if stage == "start" {
				instance.start = func() error { return errors.New(secret) }
			}
			var creations atomic.Int32
			h := newHarness(t, fakeBackend{create: func(context.Context, string, string) (core, error) {
				creations.Add(1)
				if stage == "constructor" {
					return instance, errors.New(secret)
				}
				return instance, nil
			}})
			t.Cleanup(func() {
				select {
				case <-cleanupRelease:
				default:
					close(cleanupRelease)
				}
			})
			h.initialize()
			h.send(2, "start", map[string]any{"config": "synthetic-private-config"})
			h.response(2, "")
			h.event("connecting")
			if stage == "stop" {
				h.event("connected")
				h.send(3, "stop", nil)
				h.event("disconnecting")
			}
			await(t, cleanupEntered)
			// A slow Close does not block status, checks or admission rejection.
			h.send(4, "getStatus", nil)
			h.response(4, "")
			h.send(5, "checkConfig", map[string]any{"config": "synthetic-check"})
			h.response(5, "")
			h.send(6, "start", map[string]any{"config": "must-not-construct"})
			h.response(6, "busy")
			if stage == "stop" {
				h.send(7, "stop", nil)
				h.send(8, "getStatus", nil) // Barrier for the second queued stop.
				h.response(8, "")
			}
			// Leave failed startup uncancelled to catch cleanup errors being masked
			// by the earlier constructor/Start error.
			close(cleanupRelease)
			event := h.next()
			if event.ID != nil || event.Event == nil || event.Event.Type != "status" ||
				event.Event.State != "error" || event.Event.Code != "cleanup_failed" || event.Event.Message != cleanupFailedMessage {
				t.Fatalf("cleanup failure did not take precedence: %+v", event)
			}
			if stage == "stop" {
				for _, id := range []int{3, 7} {
					if got := h.response(id, "cleanup_failed").Error.Message; got != cleanupFailedMessage {
						t.Fatalf("stop did not return the fixed cleanup diagnostic: %q", got)
					}
				}
			}
			// Retirement must end run even while the app still owns its input pipe.
			await(t, h.done)
			// Exercise the admission guard directly as if a reader had already
			// delivered another request when cleanup made the helper terminal.
			for _, method := range []string{"start", "initialize", "stop", "checkConfig", "requestPermission", "getVersion"} {
				frame, err := json.Marshal(map[string]any{
					"id": 9, "method": method,
					"arguments": map[string]any{"config": "must-not-construct", "workingPath": "test-private-app-path"},
				})
				if err != nil {
					t.Fatal(err)
				}
				h.server.handle(frame)
				h.response(9, "cleanup_failed")
			}
			h.server.handle([]byte(`{"id":10,"method":"getStatus"}`))
			var terminal status
			if err := json.Unmarshal(h.response(10, "").Result, &terminal); err != nil || terminal.State != "error" ||
				terminal.Code != "cleanup_failed" || terminal.Message != cleanupFailedMessage {
				t.Fatalf("terminal cleanup status was lost: %+v, %v", terminal, err)
			}
			if creations.Load() != 1 || instance.closed.Load() != 1 {
				t.Fatalf("retired helper reused its core: creations=%d closes=%d", creations.Load(), instance.closed.Load())
			}
			h.output.mu.Lock()
			text := h.output.text.String()
			h.output.mu.Unlock()
			if strings.Contains(text, secret) || strings.Contains(text, "synthetic-private-config") {
				t.Fatal("cleanup failure exposed backend diagnostics or configuration")
			}
		})
	}
}

func TestProtocolCleanupFailureCancelsPendingCheck(t *testing.T) {
	checkEntered, checkCleaned := make(chan struct{}), make(chan struct{})
	h := newHarness(t, fakeBackend{
		check: func(ctx context.Context, _, _ string) error {
			close(checkEntered)
			<-ctx.Done()
			close(checkCleaned)
			return ctx.Err()
		},
		create: func(context.Context, string, string) (core, error) {
			return &fakeCore{close: func() error { return errors.New("synthetic-cleanup-diagnostic") }}, nil
		},
	})
	h.initialize()
	h.send(2, "checkConfig", map[string]any{"config": "synthetic-check"})
	await(t, checkEntered)
	h.send(3, "start", map[string]any{"config": "synthetic-config"})
	h.response(3, "")
	h.event("connecting")
	h.event("connected")
	h.send(4, "stop", nil)
	h.event("disconnecting")
	h.event("error")
	h.response(4, "cleanup_failed")
	h.response(2, "invalid_config")
	await(t, checkCleaned)
	await(t, h.done)
}

func TestProtocolEOFStillWaitsForFailingCleanup(t *testing.T) {
	cleanupEntered, cleanupRelease := make(chan struct{}), make(chan struct{})
	instance := &fakeCore{close: func() error {
		close(cleanupEntered)
		<-cleanupRelease
		return errors.New("synthetic-cleanup-diagnostic")
	}}
	h := newHarness(t, fakeBackend{create: func(context.Context, string, string) (core, error) { return instance, nil }})
	t.Cleanup(func() {
		select {
		case <-cleanupRelease:
		default:
			close(cleanupRelease)
		}
	})
	h.initialize()
	h.send(2, "start", map[string]any{"config": "synthetic-config"})
	h.response(2, "")
	h.event("connecting")
	h.event("connected")
	h.input.Close()
	await(t, cleanupEntered)
	select {
	case <-h.done:
		t.Fatal("EOF returned before Close finished")
	default:
	}
	close(cleanupRelease)
	h.event("error")
	await(t, h.done)
	if instance.closed.Load() != 1 {
		t.Fatal("EOF retried failing cleanup")
	}
}
