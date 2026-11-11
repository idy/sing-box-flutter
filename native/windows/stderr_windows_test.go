// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const stderrChildMode = "SING_BOX_TEST_STDERR_CHILD"
const stderrChildReady = "stderr-probes-written\n"

var stderrSentinels = []string{
	"synthetic-secret-os-stderr",
	"synthetic-secret-go-stderr",
	"synthetic-secret-syscall-stderr",
	"synthetic-secret-windows-stderr",
	"synthetic-secret-runtime-panic",
}

// Run only in a child: redirection changes process-wide handles, and an
// unrecovered panic must exercise the actual linked runtime's diagnostic path.
func TestWindowsStderrRedirectChild(t *testing.T) {
	mode := os.Getenv(stderrChildMode)
	if mode == "" {
		return
	}
	if mode == "redirected" {
		if suppressStderr() != nil {
			os.Exit(90)
		}
	} else if mode != "inherited" {
		os.Exit(91)
	}
	handle, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil {
		os.Exit(92)
	}
	write := func(handle windows.Handle, sentinel string) {
		data := []byte(sentinel + "\n")
		var written uint32
		if windows.WriteFile(handle, data, &written, nil) != nil || int(written) != len(data) {
			os.Exit(93)
		}
	}
	write(handle, stderrSentinels[0])
	if _, err := os.Stderr.WriteString(stderrSentinels[1] + "\n"); err != nil {
		os.Exit(94)
	}
	write(windows.Handle(syscall.Stderr), stderrSentinels[2])
	write(windows.Stderr, stderrSentinels[3])
	if _, err := os.Stdout.WriteString(stderrChildReady); err != nil {
		os.Exit(95)
	}
	// A separate goroutine bypasses testing's panic recovery and forces the
	// pinned Go runtime to write to fd 2. No secret/configuration is in argv.
	go func() { panic(stderrSentinels[4]) }()
	select {}
}

func TestWindowsStderrRedirectionIncludesNativeAndRuntimeWrites(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"inherited", "redirected"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestWindowsStderrRedirectChild$")
			command.Env = append(os.Environ(), stderrChildMode+"="+mode, "GOTRACEBACK=single")
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			if ctx.Err() != nil {
				t.Fatal("stderr probe child did not exit")
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("stderr probe did not reach an unrecovered runtime panic: %v", err)
			}
			if !strings.Contains(stdout.String(), stderrChildReady) {
				t.Fatal("stderr probe did not finish its native and Go writes")
			}
			if mode == "inherited" {
				// The control prevents a broken child/capture path from making the
				// suppression assertion pass without exercising every write path.
				for _, sentinel := range stderrSentinels {
					if !strings.Contains(stderr.String(), sentinel) {
						t.Fatalf("inherited stderr did not capture probe %q", sentinel)
					}
				}
			} else if stderr.Len() != 0 {
				t.Fatalf("redirected child leaked %d bytes to inherited stderr", stderr.Len())
			}
		})
	}
}
