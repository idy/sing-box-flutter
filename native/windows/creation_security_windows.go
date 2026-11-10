// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Protected SACL creation and opening a root for full SACL reads require
// SeSecurityPrivilege. Enable only that already-held privilege on a duplicate
// of the effective caller token; never change process or outer thread authority.
func withStorageSecurityPrivilege(operation func() error) error {
	token, err := storageSecurityToken()
	if err != nil {
		return err
	}
	defer token.Close()
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := windows.SetThreadToken(nil, token); err != nil {
			runtime.UnlockOSThread()
			result <- err
			return
		}
		var operationErr error
		defer func() {
			if restoreErr := windows.RevertToSelf(); restoreErr != nil {
				operationErr = errors.Join(operationErr, restoreErr)
				// Exit while locked: Go terminates this thread rather than
				// returning an impersonating thread to its scheduler.
			} else {
				runtime.UnlockOSThread()
			}
			result <- operationErr
		}()
		operationErr = operation()
	}()
	return <-result
}

func storageSecurityToken() (windows.Token, error) {
	// Capture the effective caller before crossing to another goroutine. Only
	// ERROR_NO_TOKEN permits falling back to the process token.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var source windows.Token
	err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, true, &source)
	if err == windows.ERROR_NO_TOKEN {
		err = windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &source)
	}
	if err != nil {
		return 0, err
	}
	defer source.Close()
	if _, err := privateDirectorySecurityForToken(source); err != nil {
		return 0, err
	}
	var token windows.Token
	if err := windows.DuplicateTokenEx(source, windows.TOKEN_QUERY|windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_IMPERSONATE,
		nil, windows.SecurityImpersonation, windows.TokenImpersonation, &token); err != nil {
		return 0, err
	}
	var luid windows.LUID
	name, _ := windows.UTF16PtrFromString("SeSecurityPrivilege")
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		token.Close()
		return 0, err
	}
	state := windows.Tokenprivileges{PrivilegeCount: 1,
		Privileges: [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}}}
	if err := windows.AdjustTokenPrivileges(token, false, &state, 0, nil, nil); err != nil {
		token.Close()
		return 0, err
	}
	// AdjustTokenPrivileges may succeed with ERROR_NOT_ALL_ASSIGNED. The pinned
	// BOOL wrapper does not surface that status, so verify the actual token.
	enabled, err := tokenPrivilegeEnabled(token, luid)
	if err != nil || !enabled {
		token.Close()
		if err == nil {
			err = windows.ERROR_PRIVILEGE_NOT_HELD
		}
		return 0, err
	}
	return token, nil
}

func tokenPrivilegeEnabled(token windows.Token, luid windows.LUID) (bool, error) {
	var size uint32
	if err := windows.GetTokenInformation(token, windows.TokenPrivileges, nil, 0, &size); err != windows.ERROR_INSUFFICIENT_BUFFER {
		if err == nil {
			err = windows.ERROR_INVALID_DATA
		}
		return false, err
	}
	offset := uint32(unsafe.Offsetof(windows.Tokenprivileges{}.Privileges))
	if size < offset {
		return false, windows.ERROR_INVALID_DATA
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenPrivileges, &buffer[0], size, &size); err != nil {
		return false, err
	}
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&buffer[0]))
	if privileges.PrivilegeCount > (size-offset)/uint32(unsafe.Sizeof(windows.LUIDAndAttributes{})) {
		return false, windows.ERROR_INVALID_DATA
	}
	for _, privilege := range privileges.AllPrivileges() {
		if privilege.Luid == luid {
			return privilege.Attributes&windows.SE_PRIVILEGE_ENABLED != 0, nil
		}
	}
	return false, nil
}
