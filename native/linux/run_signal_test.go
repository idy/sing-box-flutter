// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestRunSignalLifetimeProcess(t *testing.T) {
	if os.Getenv("SINGBOX_TEST_RUN_SIGNAL_LIFETIME") != "1" {
		return
	}
	path := os.Args[len(os.Args)-1]
	os.Args = []string{os.Args[0], "--data-dir", path}
	flag.CommandLine = flag.NewFlagSet("helper", flag.ContinueOnError)
	code := run()
	fmt.Fprintln(os.Stdout, "run-returned")
	// Expose the run-return/os.Exit boundary without relying on scheduling.
	gate := os.NewFile(3, "exit-boundary")
	var release [1]byte
	if _, err := io.ReadFull(gate, release[:]); err != nil {
		os.Exit(4)
	}
	os.Exit(code)
}

func TestLateSIGTERMAfterEOFDoesNotChangeExitStatus(t *testing.T) {
	_, path := ownedShutdownState(t)
	var output lockedOutput
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gateRead.Close()
	defer gateWrite.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestRunSignalLifetimeProcess$", "--", path)
	command.Env = append(os.Environ(), "SINGBOX_TEST_RUN_SIGNAL_LIFETIME=1")
	command.Stdout = struct{ io.Writer }{&output}
	command.Stderr = io.Discard
	command.ExtraFiles = []*os.File{gateRead}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	_, _ = fmt.Fprintln(stdin, `{"id":1,"method":"getInfo"}`)
	awaitOutput(t, &output, `"process_start_time"`)
	_ = stdin.Close()
	awaitOutput(t, &output, "run-returned")
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_, _ = gateWrite.Write([]byte{1})
	err = command.Wait()
	waited = true
	if err != nil {
		t.Fatalf("late SIGTERM changed a completed orderly exit: %v", err)
	}
}
