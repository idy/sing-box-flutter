// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

const supervisorShutdownTimeout = finalWorkerDrainTimeout + 3*time.Second

// Only temporary-state helpers need a supervisor. It retains the original
// directory pins while the engine runs, and reaps state after Wait proves that
// every engine thread is dead, even following failed cleanup or a forced exit.
func superviseTemporaryState(path string, input io.Reader, signals <-chan os.Signal, worker *exec.Cmd) int {
	state, err := pinTemporaryStateDirectory(path)
	if err != nil {
		return 2
	}
	defer state.Close()
	stdin, err := worker.StdinPipe()
	if err != nil {
		_ = state.Remove()
		return 2
	}
	if err := worker.Start(); err != nil {
		_ = stdin.Close()
		_ = state.Remove()
		return 2
	}
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
