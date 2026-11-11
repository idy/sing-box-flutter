// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	libbox "github.com/sagernet/sing-box/experimental/libbox"
)

func ownedShutdownState(t *testing.T) (*temporaryStateDirectory, string) {
	t.Helper()
	path, err := os.MkdirTemp(stateTestRoot(t), "shutdown-")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := prepareStateDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := pinTemporaryStateDirectory(canonical)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if err := os.WriteFile(filepath.Join(path, "identity"), []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	return state, path
}

func TestFinalShutdownBoundsRetiredWorkerAndPreservesState(t *testing.T) {
	for _, construction := range []bool{false, true} {
		for _, signalExit := range []bool{false, true} {
			t.Run(fmt.Sprintf("constructor=%v/signal=%v", construction, signalExit), func(t *testing.T) {
				var output lockedOutput
				entered, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				service := &fakeService{}
				s := testServer(&output, service)
				s.create = func(context.Context, *libbox.DesktopConfig) (coreService, error) {
					if construction {
						close(entered)
						<-release
					} else {
						service.start = func() error { close(entered); <-release; return nil }
					}
					return service, nil
				}
				state, path := ownedShutdownState(t)
				input, writer := io.Pipe()
				signals := make(chan os.Signal, 1)
				served := make(chan struct{})
				t.Cleanup(func() {
					releaseOnce.Do(func() { close(release) })
					_ = writer.Close()
					_ = input.Close()
					await(t, served, "server did not exit")
					if err := s.waitForWorkers(); err != nil {
						t.Errorf("released worker did not drain: %v", err)
					}
				})
				go func() { s.serve(input, signals); close(served) }()
				_, _ = fmt.Fprintln(writer, `{"id":1,"method":"start"}`)
				await(t, entered, "startup did not block")
				if signalExit {
					signals <- syscall.SIGTERM
				} else {
					_ = writer.Close()
				}
				await(t, served, "EOF/SIGTERM did not stop request admission")
				started := time.Now()
				if err := s.finishTemporaryState(state); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("uncooperative worker must time out, got %v", err)
				}
				if time.Since(started) > finalWorkerDrainTimeout+2*time.Second {
					t.Fatal("final worker drain exceeded its shutdown bound")
				}
				if contents, err := os.ReadFile(filepath.Join(path, "identity")); err != nil || string(contents) != "preserved" {
					t.Fatalf("timed-out worker state was deleted: %q, %v", contents, err)
				}
				if len(state.directories) != 0 {
					t.Fatal("shutdown timeout did not release directory pins")
				}
			})
		}
	}
}

func TestFinalShutdownPreservesFailedCleanupState(t *testing.T) {
	var output lockedOutput
	service := &fakeService{closeErr: errors.New("secret-close-error")}
	s := testServer(&output, service)
	state, path := ownedShutdownState(t)
	s.handle(context.Background(), request{ID: 1, Method: "start"})
	if err := s.close(); !errors.Is(err, errCleanupFailed) {
		t.Fatalf("fixture cleanup unexpectedly succeeded: %v", err)
	}
	if err := s.finishTemporaryState(state); !errors.Is(err, errCleanupFailed) {
		t.Fatalf("shutdown forgot failed native cleanup: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(path, "identity")); err != nil || string(contents) != "preserved" {
		t.Fatalf("failed-cleanup state was deleted: %q, %v", contents, err)
	}
	if len(state.directories) != 0 {
		t.Fatal("failed cleanup did not release directory pins")
	}
}

func TestFinalShutdownRemovesSuccessfullyClosedState(t *testing.T) {
	var output lockedOutput
	s := testServer(&output, &fakeService{})
	state, path := ownedShutdownState(t)
	s.handle(context.Background(), request{ID: 1, Method: "start"})
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if err := s.finishTemporaryState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("successful shutdown left temporary state: %v", err)
	}
}
