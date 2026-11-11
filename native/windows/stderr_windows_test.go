// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

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

func TestWindowsStderrIgnoresRemappedNUL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "captured-stderr.log")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	device, err := windows.UTF16PtrFromString("NUL")
	if err != nil {
		t.Fatal(err)
	}
	target, err := windows.UTF16PtrFromString(`\??\` + path)
	if err != nil {
		t.Fatal(err)
	}
	define := windows.NewLazySystemDLL("kernel32.dll").NewProc("DefineDosDeviceW")
	set := func(flags uintptr) error {
		result, _, callErr := define.Call(flags, uintptr(unsafe.Pointer(device)), uintptr(unsafe.Pointer(target)))
		if result == 0 {
			return callErr
		}
		return nil
	}
	const rawTarget, removeDefinition, exactMatch, noBroadcast = 1, 2, 4, 8
	if err := set(rawTarget | noBroadcast); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set(rawTarget | removeDefinition | exactMatch | noBroadcast); err != nil {
			t.Errorf("restore NUL alias: %v", err)
		}
	})
	// The control proves that ordinary DOS NUL opens reach the attacker file.
	const control = "remapped-null-control\n"
	ordinary, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := ordinary.WriteString(control)
	closeErr := ordinary.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("write remapped NUL control: %v, %v", writeErr, closeErr)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != control {
		t.Fatalf("NUL was not remapped to the control file: %q, %v", data, err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestWindowsStderrRedirectChild$")
	command.Env = append(os.Environ(), stderrChildMode+"=redirected", "GOTRACEBACK=single")
	// Avoid an implicit DOS NUL open for the child's unused stdin.
	command.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	var exit *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 2 ||
		!strings.Contains(stdout.String(), stderrChildReady) {
		t.Fatalf("remapped-NUL child did not exercise native/runtime writes: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("trusted null sink leaked %d bytes to inherited stderr", stderr.Len())
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != control {
		t.Fatalf("trusted null sink wrote diagnostics to remapped NUL: %d bytes, %v", len(data), err)
	}
}
