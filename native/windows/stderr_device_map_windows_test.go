// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The child owns an unnamed container and an ordinary named directory below it.
// Only that child uses the directory as its device map. No name is added to the
// shared object namespace, and process exit releases the complete private tree.
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
	directoryName, err := windows.NewNTUnicodeString("Devices")
	if err != nil {
		return err
	}
	linkName, err := windows.NewNTUnicodeString("NUL")
	if err != nil {
		return err
	}
	ntdll := windows.NewLazySystemDLL("ntdll.dll")
	createDirectory := ntdll.NewProc("NtCreateDirectoryObject")
	createLink := ntdll.NewProc("NtCreateSymbolicLinkObject")
	for _, procedure := range []*windows.LazyProc{createDirectory, createLink} {
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
	var container, directory, link windows.Handle
	keep := false
	defer func() {
		if !keep {
			for _, handle := range []windows.Handle{link, directory, container} {
				if handle != 0 {
					windows.CloseHandle(handle)
				}
			}
		}
	}()
	const directoryAllAccess = 0x000f000f
	// ProcessDeviceMap uses an ordinary directory, not an anonymous shadow
	// directory created with NtCreateDirectoryObjectEx. A private container gives
	// the map a named leaf without exposing either object through a global path.
	result, _, _ := createDirectory.Call(uintptr(unsafe.Pointer(&container)), directoryAllAccess, 0)
	if err := statusError(result); err != nil {
		return fmt.Errorf("create private container: %w", err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: container,
		ObjectName:    directoryName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	result, _, _ = createDirectory.Call(uintptr(unsafe.Pointer(&directory)), directoryAllAccess,
		uintptr(unsafe.Pointer(&attributes)))
	if err := statusError(result); err != nil {
		return fmt.Errorf("create private device directory: %w", err)
	}
	attributes.RootDirectory = directory
	attributes.ObjectName = linkName
	const symbolicLinkQuery = 1
	result, _, _ = createLink.Call(uintptr(unsafe.Pointer(&link)), symbolicLinkQuery,
		uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(target)))
	if err := statusError(result); err != nil {
		return fmt.Errorf("create private NUL link: %w", err)
	}
	// The setter consumes a HANDLE, not the larger query union. After switching,
	// this child needs no DOS drives: the capture link uses its native device path
	// and the production sink opens \Device\Null directly.
	err = windows.NtSetInformationProcess(windows.CurrentProcess(), windows.ProcessDeviceMap,
		unsafe.Pointer(&directory), uint32(unsafe.Sizeof(directory)))
	if err != nil {
		return fmt.Errorf("set child process device map: %w", err)
	}
	keep = true // Retain all three handles until panic or forced process exit.
	return nil
}
