// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Runs in a real child process, so Wait (rather than a mock channel) proves
// that even a timed-out writer cannot access state before the supervisor reaps it.
func TestTemporaryWorkerProcess(t *testing.T) {
	if os.Getenv("SINGBOX_TEST_STATE_WORKER") != "1" {
		return
	}
	path, scenario := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	if err := os.WriteFile(filepath.Join(path, "worker-identity"), []byte("active"), 0o600); err != nil {
		os.Exit(4)
	}
	if scenario == "timeout" || scenario == "stuck" || scenario == "cleanup-failed" {
		go func() {
			for {
				if os.WriteFile(filepath.Join(path, "worker-write"), []byte("active"), 0o600) != nil {
					os.Exit(4) // State must remain writable until process death.
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	fmt.Fprintln(os.Stdout, "ready")
	if scenario == "signal" {
		<-signals
		os.Exit(0)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	switch scenario {
	case "timeout":
		s := &server{}
		s.workers.Add(1) // Deliberately never completes before process exit.
		if s.waitForWorkers() != context.DeadlineExceeded {
			os.Exit(4)
		}
		os.Exit(2)
	case "cleanup-failed":
		s := &server{cleanupFailed: true}
		if s.waitForWorkers() != errCleanupFailed {
			os.Exit(4)
		}
		os.Exit(2)
	case "stuck":
		for {
			time.Sleep(time.Second)
		}
	case "crash":
		os.Exit(3)
	}
	os.Exit(0)
}

func temporaryWorkerCommand(path, scenario string, output io.Writer) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestTemporaryWorkerProcess$", "--", path, scenario)
	command.Env = append(os.Environ(), "SINGBOX_TEST_STATE_WORKER=1")
	// Do not promote bytes.Buffer.ReadFrom through lockedOutput: all child
	// output must pass through its synchronized Write and readiness signal.
	command.Stdout = struct{ io.Writer }{output}
	command.Stderr = io.Discard
	return command
}

func TestSupervisorReapsOnlyAfterWorkerExit(t *testing.T) {
	for _, scenario := range []string{"normal", "cleanup-failed", "timeout", "crash", "signal", "stuck"} {
		t.Run(scenario, func(t *testing.T) {
			_, path := ownedShutdownState(t)
			var output lockedOutput
			input, writer := io.Pipe()
			defer input.Close()
			defer writer.Close()
			signals := make(chan os.Signal, 1)
			command := temporaryWorkerCommand(path, scenario, &output)
			result := make(chan int, 1)
			go func() { result <- superviseTemporaryState(path, input, signals, command) }()
			awaitOutput(t, &output, "ready")
			if data, err := os.ReadFile(filepath.Join(path, "worker-identity")); err != nil || string(data) != "active" {
				t.Fatalf("live worker state was deleted: %q, %v", data, err)
			}
			select {
			case code := <-result:
				t.Fatalf("supervisor exited while worker still owned state: %d", code)
			default:
			}
			if scenario == "signal" {
				signals <- syscall.SIGTERM
			} else {
				_ = writer.Close()
			}
			select {
			case code := <-result:
				expected := map[string]int{"normal": 0, "cleanup-failed": 2, "timeout": 2, "crash": 3, "signal": 0, "stuck": 2}[scenario]
				if code != expected {
					t.Fatalf("worker outcome changed: got %d, want %d", code, expected)
				}
			case <-time.After(supervisorShutdownTimeout + 3*time.Second):
				_ = command.Process.Kill()
				t.Fatal("supervisor did not bound final worker shutdown")
			}
			if command.ProcessState == nil || !command.ProcessState.Exited() && scenario != "stuck" {
				t.Fatal("supervisor did not wait for actual worker exit")
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("post-exit temporary state leaked: %v", err)
			}
		})
	}
}

func TestSupervisorRefusesReplacementAfterWorkerExit(t *testing.T) {
	_, path := ownedShutdownState(t)
	var output lockedOutput
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	command := temporaryWorkerCommand(path, "normal", &output)
	result := make(chan int, 1)
	go func() { result <- superviseTemporaryState(path, input, nil, command) }()
	awaitOutput(t, &output, "ready")
	original := path + "-original"
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "replacement")
	if err := os.WriteFile(marker, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case code := <-result:
		if code != 2 {
			t.Fatalf("replaced state must be refused: %d", code)
		}
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("replacement refusal did not complete")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "untouched" {
		t.Fatalf("replacement was modified: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(original, "worker-identity")); err != nil {
		t.Fatalf("renamed original was unexpectedly removed: %v", err)
	}
}

func TestSupervisorCleansStateWhenWorkerCannotStart(t *testing.T) {
	_, path := ownedShutdownState(t)
	code := superviseTemporaryState(path, nil, nil, exec.Command(filepath.Join(path, "missing-worker")))
	if code != 2 {
		t.Fatalf("missing worker must fail startup: %d", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unused temporary state leaked: %v", err)
	}
}
