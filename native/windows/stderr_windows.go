// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func redirectProcessStderr(sink *os.File) error {
	handle := windows.Handle(sink.Fd())
	// The pinned Tailscale Go runtime's runtime/os_windows.go write1 resolves
	// fd 2 through GetStdHandle on every write, including unrecovered panics.
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, handle); err != nil {
		return err
	}
	// These packages also cache the inherited handle during initialization.
	syscall.Stderr = syscall.Handle(handle)
	windows.Stderr = handle
	return nil
}
