// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openProcessStderrSink() (*os.File, error) {
	// NUL and even \\?\NUL resolve through the caller's mutable DOS device map.
	// The kernel's absolute Null device is outside that namespace; do not follow
	// any Object Manager or filesystem reparse point while opening it.
	name, err := windows.NewNTUnicodeString(`\Device\Null`)
	if err != nil {
		return nil, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		ObjectName: name,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, windows.FILE_WRITE_DATA|windows.SYNCHRONIZE,
		&attributes, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		if handle != 0 && handle != windows.InvalidHandle {
			windows.CloseHandle(handle)
		}
		return nil, err
	}
	kind, err := syscall.GetFileType(syscall.Handle(handle))
	if err != nil || kind != syscall.FILE_TYPE_CHAR {
		windows.CloseHandle(handle)
		if err != nil {
			return nil, err
		}
		return nil, syscall.EINVAL
	}
	return os.NewFile(uintptr(handle), `\Device\Null`), nil
}

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
