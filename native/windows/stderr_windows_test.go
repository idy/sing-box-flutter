// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const stderrChildMode = "SING_BOX_TEST_STDERR_CHILD"
const stderrChildReady = "stderr-probes-written\n"
const stderrMapReady = "isolated-stderr-map-ready\n"
const stderrMapTarget = "SING_BOX_TEST_STDERR_MAP_TARGET"
const stderrMapControl = "remapped-null-control\n"

func prepareStderrPrivateMapChild() error {
	capture, err := os.OpenFile(os.Getenv(stderrMapTarget), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer capture.Close()
	if err := installStderrPrivateDeviceMap(capture); err != nil {
		return err
	}
	ordinary, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, writeErr := ordinary.WriteString(stderrMapControl)
	closeErr := ordinary.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if _, err := os.Stdout.WriteString(stderrMapReady); err != nil {
		return err
	}
	// The parent checks its own NUL while this private map is active, then
	// either releases the panic probe or kills this child before Go cleanup.
	var release [1]byte
	_, err = io.ReadFull(os.Stdin, release[:])
	return err
}

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
	if mode == "isolated-remapped" {
		if err := prepareStderrPrivateMapChild(); err != nil {
			fmt.Fprintln(os.Stdout, "isolated device-map setup failed:", err)
			os.Exit(96)
		}
		mode = "redirected"
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

type stderrIsolatedOutput struct {
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func (output *stderrIsolatedOutput) Write(data []byte) (int, error) {
	count, err := output.buffer.Write(data)
	if bytes.Contains(output.buffer.Bytes(), []byte(stderrMapReady)) {
		output.once.Do(func() { close(output.ready) })
	}
	return count, err
}

func (output *stderrIsolatedOutput) String() string { return output.buffer.String() }

func assertParentNULIsCharacterDevice(t *testing.T) {
	t.Helper()
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	kind, err := syscall.GetFileType(syscall.Handle(file.Fd()))
	if err != nil || kind != syscall.FILE_TYPE_CHAR {
		t.Fatalf("child device map affected parent's NUL: type=%d error=%v", kind, err)
	}
}

func TestWindowsStderrIgnoresRemappedNUL(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, terminate := range []bool{false, true} {
		name := "panic"
		if terminate {
			name = "forced-termination"
		}
		t.Run(name, func(t *testing.T) {
			assertParentNULIsCharacterDevice(t)
			path := filepath.Join(t.TempDir(), "captured-stderr.log")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestWindowsStderrRedirectChild$")
			command.Env = append(os.Environ(), stderrChildMode+"=isolated-remapped",
				stderrMapTarget+"="+path, "GOTRACEBACK=single")
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout := &stderrIsolatedOutput{ready: make(chan struct{})}
			var stderr bytes.Buffer
			command.Stdout, command.Stderr = stdout, &stderr
			if err := command.Start(); err != nil {
				stdin.Close()
				t.Fatal(err)
			}
			done := make(chan struct{})
			var runErr error
			go func() {
				runErr = command.Wait()
				close(done)
			}()
			t.Cleanup(func() {
				_ = command.Process.Kill()
				_ = stdin.Close()
				<-done
			})
			select {
			case <-stdout.ready:
			case <-done:
				t.Fatalf("isolated map child failed before handshake: %v, %s", runErr, stdout.String())
			case <-ctx.Done():
				t.Fatal("isolated map child did not become ready")
			}
			// The child is alive with its private NUL alias. Parent/logon-session
			// opens must still resolve to a character device, not the capture file.
			assertParentNULIsCharacterDevice(t)
			if data, err := os.ReadFile(path); err != nil || string(data) != stderrMapControl {
				t.Fatalf("child did not exercise a private DOS NUL mapping: %q, %v", data, err)
			}
			if terminate {
				if err := command.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := stdin.Write([]byte{1}); err != nil {
					t.Fatal(err)
				}
			}
			_ = stdin.Close()
			<-done
			if ctx.Err() != nil {
				t.Fatal("isolated map child did not exit")
			}
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) || (!terminate && exit.ExitCode() != 2) {
				t.Fatalf("isolated map child ended unexpectedly: %v", runErr)
			}
			if !terminate && !strings.Contains(stdout.String(), stderrChildReady) {
				t.Fatal("private-map child did not exercise OS/Go/panic writes")
			}
			if stderr.Len() != 0 {
				t.Fatalf("trusted null sink leaked %d bytes to inherited stderr", stderr.Len())
			}
			assertParentNULIsCharacterDevice(t)
			if data, err := os.ReadFile(path); err != nil || string(data) != stderrMapControl {
				t.Fatalf("trusted null sink wrote diagnostics to private NUL: %d bytes, %v", len(data), err)
			}
		})
	}
}
