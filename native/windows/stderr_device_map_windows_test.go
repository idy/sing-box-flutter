// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A test child owns an unnamed directory and a process-specific device map.
// Nothing is inserted into the logon session's DOS directory. Kernel references
// disappear on child exit, including forced termination before Go cleanup runs.
func installStderrPrivateDeviceMap(capture *os.File) (err error) {
	var buffer [32768]uint16
	size, err := windows.GetFinalPathNameByHandle(windows.Handle(capture.Fd()), &buffer[0], uint32(len(buffer)), 2)
	if err != nil {
		return fmt.Errorf("capture native path lookup failed: %w", err)
	}
	if size == 0 || size >= uint32(len(buffer)) {
		return fmt.Errorf("capture native path length is invalid: %d", size)
	}
	target, err := windows.NewNTUnicodeString(windows.UTF16ToString(buffer[:size]))
	if err != nil {
		return err
	}
	globalName, err := windows.NewNTUnicodeString(`\GLOBAL??`)
	if err != nil {
		return err
	}
	linkName, err := windows.NewNTUnicodeString("NUL")
	if err != nil {
		return err
	}
	ntdll := windows.NewLazySystemDLL("ntdll.dll")
	openDirectory := ntdll.NewProc("NtOpenDirectoryObject")
	createDirectory := ntdll.NewProc("NtCreateDirectoryObjectEx")
	createLink := ntdll.NewProc("NtCreateSymbolicLinkObject")
	for _, procedure := range []*windows.LazyProc{openDirectory, createDirectory, createLink} {
		if err := procedure.Find(); err != nil {
			return err
		}
	}
	statusError := func(result uintptr) error {
		if int32(result) < 0 {
			return windows.NTStatus(result)
		}
		return nil
	}
	var global, directory, link windows.Handle
	keep := false
	defer func() {
		if !keep {
			for _, handle := range []windows.Handle{link, directory, global} {
				if handle != 0 {
					windows.CloseHandle(handle)
				}
			}
		}
	}()
	attributes := windows.OBJECT_ATTRIBUTES{
		ObjectName: globalName,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	const directoryQueryTraverse = 3
	// Keep pointer conversions at the annotated syscall Call sites, not in a
	// uintptr-forwarding helper that could lose pointer liveness on stack growth.
	result, _, _ := openDirectory.Call(uintptr(unsafe.Pointer(&global)), directoryQueryTraverse,
		uintptr(unsafe.Pointer(&attributes)))
	if err := statusError(result); err != nil {
		return fmt.Errorf("open global fallback directory: %w", err)
	}
	// NULL ObjectAttributes creates an unnamed private directory. The read-only
	// shadow preserves system drive lookup without changing any global entry.
	const directoryAllAccess = 0x000f000f
	result, _, _ = createDirectory.Call(uintptr(unsafe.Pointer(&directory)), directoryAllAccess,
		0, uintptr(global), 0)
	if err := statusError(result); err != nil {
		return fmt.Errorf("create unnamed device directory: %w", err)
	}
	attributes.RootDirectory = directory
	attributes.ObjectName = linkName
	const symbolicLinkQuery = 1
	result, _, _ = createLink.Call(uintptr(unsafe.Pointer(&link)), symbolicLinkQuery,
		uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(target)))
	if err := statusError(result); err != nil {
		return fmt.Errorf("create private NUL link: %w", err)
	}
	// Set uses the HANDLE member. Some supported native interfaces require the
	// complete union size (the query member is ULONG + UCHAR[32], aligned to 8).
	information := struct {
		Directory windows.Handle
		Reserved  [32]byte
	}{Directory: directory}
	err = windows.NtSetInformationProcess(windows.CurrentProcess(), windows.ProcessDeviceMap,
		unsafe.Pointer(&information), uint32(unsafe.Sizeof(information.Directory)))
	if err == windows.STATUS_INFO_LENGTH_MISMATCH {
		err = windows.NtSetInformationProcess(windows.CurrentProcess(), windows.ProcessDeviceMap,
			unsafe.Pointer(&information), uint32(unsafe.Sizeof(information)))
	}
	if err != nil {
		return fmt.Errorf("set child process device map: %w", err)
	}
	// Deliberately retain these handles until this child exits. No parent or
	// logon-session alias exists to restore, even if the child is killed.
	keep = true
	return nil
}
