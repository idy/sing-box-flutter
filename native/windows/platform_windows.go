// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func isElevated() bool {
	// Use the process token, not an inherited/impersonated thread token. A
	// filtered administrator token is insufficient for creating a Wintun device.
	var token windows.Token
	if windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &token) != nil {
		return false
	}
	defer token.Close()
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil || !token.IsElevated() {
		return false
	}
	var impersonation windows.Token
	if windows.DuplicateTokenEx(token, windows.TOKEN_QUERY, nil, windows.SecurityIdentification, windows.TokenImpersonation, &impersonation) != nil {
		return false
	}
	defer impersonation.Close()
	member, err := impersonation.IsMember(administrators)
	return err == nil && member
}

// An exclusive open handle is released by the OS on process death, so there
// is no stale lock file to recover. Keep ownership through route cleanup.
func lockWorkingDirectory(path string) (io.Closer, error) {
	name, err := windows.UTF16PtrFromString(filepath.Join(path, "core.lock"))
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), "core.lock"), nil
}

func privateDirectory(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	// Protected, inheritable DACL: only this user and SYSTEM. os.FileMode alone
	// does not enforce private directories on Windows.
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	err = windows.CreateDirectory(name, &attributes)
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errDirectories
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
