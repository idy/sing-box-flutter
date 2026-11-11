// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const supervisorShutdownTimeout = finalWorkerDrainTimeout + 3*time.Second

// Exec of a capability-bearing helper can clear the pre-exec parent-death
// signal. Re-arm it before any engine resources exist, checking both sides of
// prctl to close the race with supervisor exit. Keep this worker thread alive
// until main exits: the setting belongs to the calling Linux task.
func bindTemporaryWorker(parent int) error {
	runtime.LockOSThread()
	if parent <= 0 || os.Getppid() != parent {
		runtime.UnlockOSThread()
		return errors.New("temporary supervisor exited")
	}
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0); err != nil {
		runtime.UnlockOSThread()
		return err
	}
	if os.Getppid() != parent {
		runtime.UnlockOSThread()
		return errors.New("temporary supervisor exited")
	}
	return nil
}

// Watch the upstream pipe itself, not the forwarding goroutine. POLLHUP is
// delivered even with unread bytes, so a full worker pipe cannot hide engine
// exit. A separate cancellation pipe releases the poller on supervisor return.
func watchParentInput(input *os.File) (<-chan struct{}, func(), error) {
	stopRead, stopWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	closed := make(chan struct{})
	fds := []unix.PollFd{{Fd: int32(input.Fd())}, {Fd: int32(stopRead.Fd())}}
	go func() {
		defer stopRead.Close()
		for {
			_, err := unix.Poll(fds, -1)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if fds[1].Revents != 0 {
				return
			}
			if err != nil || fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
				close(closed)
				return
			}
		}
	}()
	return closed, func() { _ = stopWrite.Close() }, nil
}

// Only temporary-state helpers need a supervisor. It retains the original
// directory pins while the engine runs, and reaps state after Wait proves that
// every engine thread is dead, even following failed cleanup or a forced exit.
func superviseTemporaryState(path string, input *os.File, signals <-chan os.Signal, worker *exec.Cmd) int {
	// Linux parent-death signals follow the creating thread, not just its process.
	// Never let Go retire that thread while its worker is still alive.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	state, err := pinTemporaryStateDirectory(path)
	if err != nil {
		return 2
	}
	defer state.Close()
	if input == nil {
		_ = state.Remove()
		return 2
	}
	stdin, err := worker.StdinPipe()
	if err != nil {
		_ = state.Remove()
		return 2
	}
	if worker.SysProcAttr == nil {
		worker.SysProcAttr = &syscall.SysProcAttr{}
	}
	worker.SysProcAttr.Pdeathsig = syscall.SIGKILL
	if err := worker.Start(); err != nil {
		_ = stdin.Close()
		_ = state.Remove()
		return 2
	}
	parentClosed, stopWatching, err := watchParentInput(input)
	if err != nil {
		_ = stdin.Close()
		_ = worker.Process.Kill()
		_ = worker.Wait()
		_ = state.Remove()
		return 2
	}
	defer stopWatching()
	inputClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(stdin, input)
		_ = stdin.Close()
		close(inputClosed)
	}()
	exited := make(chan error, 1)
	go func() { exited <- worker.Wait() }()
	var deadline <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	beginShutdown := func() {
		if timer == nil {
			timer = time.NewTimer(supervisorShutdownTimeout)
			deadline = timer.C
		}
	}
	for {
		select {
		case err := <-exited:
			// Never remove before Wait, including when Close or the worker drain
			// failed. Remove refuses renamed/replaced paths and outside links.
			if state.Remove() != nil {
				return 2
			}
			if err == nil {
				return 0
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() > 0 {
				return exit.ExitCode()
			}
			return 2
		case <-parentClosed:
			parentClosed = nil
			beginShutdown()
		case <-inputClosed:
			inputClosed = nil
			beginShutdown()
		case signal, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			_ = worker.Process.Signal(signal)
			beginShutdown()
		case <-deadline:
			// A stuck Close must not keep the supervisor alive after its engine.
			// Cleanup still waits for actual process death, never just a timer.
			_ = worker.Process.Kill()
			deadline = nil
		}
	}
}
