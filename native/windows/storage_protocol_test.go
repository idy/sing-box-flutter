// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
)

type protocolStorageLease struct{ closed atomic.Int32 }

func (s *protocolStorageLease) Close() error { s.closed.Add(1); return nil }

func TestProtocolStorageOwnershipCoversCoreAndCheckCleanup(t *testing.T) {
	lease := &protocolStorageLease{}
	checkEntered, checkCleaning, checkRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	coreCleaning, coreRelease, coreCleaned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b := fakeBackend{
		check: func(ctx context.Context, _, _ string) error {
			if ctx.Value(storageContextKey{}) != lease {
				t.Error("configuration check lost its storage owner")
			}
			close(checkEntered)
			<-ctx.Done()
			close(checkCleaning)
			<-checkRelease
			return ctx.Err()
		},
		create: func(ctx context.Context, _, _ string) (core, error) {
			if ctx.Value(storageContextKey{}) != lease {
				t.Error("connection lost its storage owner")
			}
			return &fakeCore{close: func() error {
				close(coreCleaning)
				<-coreRelease
				close(coreCleaned)
				return nil
			}}, nil
		},
	}
	h := newHarness(t, b)
	var acquired atomic.Int32
	h.server.storage = func(string) (io.Closer, error) { acquired.Add(1); return lease, nil }
	t.Cleanup(func() {
		for _, release := range []chan struct{}{coreRelease, checkRelease} {
			select {
			case <-release:
			default:
				close(release)
			}
		}
	})
	h.initialize()
	h.initialize()
	if acquired.Load() != 1 {
		t.Fatal("idempotent initialize reacquired storage")
	}
	h.send(2, "checkConfig", map[string]any{"config": "check"})
	await(t, checkEntered)
	h.send(3, "start", map[string]any{"config": "start"})
	h.response(3, "")
	h.event("connecting")
	h.event("connected")
	h.input.Close()
	await(t, coreCleaning)
	await(t, checkCleaning)
	if lease.closed.Load() != 0 {
		t.Fatal("released storage while workers still used it")
	}
	close(coreRelease)
	await(t, coreCleaned)
	if lease.closed.Load() != 0 {
		t.Fatal("released storage before configuration check cleanup")
	}
	close(checkRelease)
	await(t, h.done)
	if lease.closed.Load() != 1 {
		t.Fatal("storage was not released exactly once after cleanup")
	}
}

func TestProtocolStorageFailureReleasesPartialLeaseAndAllowsRetry(t *testing.T) {
	h := newHarness(t, fakeBackend{})
	partial, owned := &protocolStorageLease{}, &protocolStorageLease{}
	var attempts atomic.Int32
	h.server.storage = func(string) (io.Closer, error) {
		if attempts.Add(1) == 1 {
			return partial, errors.New("private storage details")
		}
		return owned, nil
	}
	h.send(1, "initialize", map[string]any{"workingPath": "test-private-app-path"})
	h.response(1, "initialize_failed")
	if partial.closed.Load() != 1 {
		t.Fatal("failed initializer retained its partial storage lease")
	}
	h.initialize()
	h.input.Close()
	await(t, h.done)
	if owned.closed.Load() != 1 {
		t.Fatal("retry did not release its storage lease")
	}
}
