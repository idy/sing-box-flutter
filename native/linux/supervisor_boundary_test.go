// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSupervisorDetectsParentCloseWithBlockedWorkerInput(t *testing.T) {
	_, path := ownedShutdownState(t)
	var output lockedOutput
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	worker := temporaryWorkerCommand(path, "not-reading", &output)
	result := make(chan int, 1)
	go func() { result <- superviseTemporaryState(path, input, nil, worker) }()
	awaitOutput(t, &output, "ready")
	written := make(chan error, 1)
	go func() {
		_, err := writer.Write(bytes.Repeat([]byte("x"), 4*1024*1024))
		written <- err
	}()
	select {
	case err := <-written:
		t.Fatalf("fixture did not fill the blocked input pipeline: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	_ = writer.Close()
	select {
	case code := <-result:
		if code != 2 {
			t.Fatalf("stuck worker must be terminated: %d", code)
		}
	case <-time.After(supervisorShutdownTimeout + 3*time.Second):
		_ = worker.Process.Kill()
		t.Fatal("parent EOF was hidden by the blocked worker write")
	}
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("closing upstream did not release its pending write")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("backpressured shutdown leaked state: %v", err)
	}
}

func TestTemporarySupervisorProcess(t *testing.T) {
	if os.Getenv("SINGBOX_TEST_STATE_SUPERVISOR") != "1" {
		return
	}
	path := os.Args[len(os.Args)-1]
	worker := temporaryWorkerCommand(path, "not-reading", os.Stdout)
	os.Exit(superviseTemporaryState(path, os.Stdin, nil, worker))
}

func TestSupervisorDeathKillsItsWorker(t *testing.T) {
	_, path := ownedShutdownState(t)
	var output lockedOutput
	command := exec.Command(os.Args[0], "-test.run=^TestTemporarySupervisorProcess$", "--", path)
	command.Env = append(os.Environ(), "SINGBOX_TEST_STATE_SUPERVISOR=1")
	command.Stdout = struct{ io.Writer }{&output}
	command.Stderr = io.Discard
	// If the regression leaves a worker holding stdout, do not let os/exec's
	// output-copy drain hide the orphan from the pidfd assertion below.
	command.WaitDelay = 100 * time.Millisecond
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	awaitOutput(t, &output, "ready")
	pidBytes, err := os.ReadFile(filepath.Join(path, "worker-pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pidfd)
	defer unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	waited = true
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, 1000); err != nil || n != 1 || fds[0].Revents&unix.POLLIN == 0 {
		t.Fatalf("worker survived supervisor SIGKILL: events=%v, err=%v", fds, err)
	}
}
