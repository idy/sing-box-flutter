// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"path/filepath"
	"strings"
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

func privateDirectory(path string) error {
	_, parts, err := secureDirectoryPath(path)
	if err != nil || len(parts) == 0 {
		return errDirectories
	}
	descriptor, err := privateDirectorySecurity()
	if err != nil {
		return err
	}
	directories, err := openDirectoryHierarchy(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directories.close()
	handle, err := directories.openChild(directories.last(), parts[len(parts)-1], true, descriptor)
	if err != nil {
		return err
	}
	return setPrivateDirectorySecurity(handle, descriptor)
}

func privateDirectorySecurity() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	var size uint32
	err = windows.GetTokenInformation(windows.GetCurrentProcessToken(), windows.TokenIntegrityLevel, nil, 0, &size)
	if err != windows.ERROR_INSUFFICIENT_BUFFER || size < uint32(unsafe.Sizeof(windows.Tokenmandatorylabel{})) {
		if err != nil {
			return nil, err
		}
		return nil, errDirectories
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(windows.GetCurrentProcessToken(), windows.TokenIntegrityLevel, &buffer[0], size, &size); err != nil {
		return nil, err
	}
	label := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buffer[0])).Label.Sid.String()
	// Keep user+SYSTEM full control. An inheritable no-write-up label at the
	// process integrity level (high for the elevated helper) prevents a lower
	// integrity process with the same user SID from changing managed state.
	// This is local to these objects; no token privilege or global policy changes.
	return windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + user.User.Sid.String() + ")S:(ML;OICI;NW;;;" + label + ")")
}

func setPrivateDirectorySecurity(handle windows.Handle, descriptor *windows.SECURITY_DESCRIPTOR) error {
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	label, _, err := descriptor.SACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION,
		nil, nil, dacl, label)
}

// Accept only clean, drive-absolute paths and literal directory components.
// UNC/device namespaces, ADS, DOS device names and Win32-normalized aliases
// cannot be given equivalent, unambiguous handle-relative semantics here.
func secureDirectoryPath(path string) (string, []string, error) {
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' ||
		!((volume[0] >= 'A' && volume[0] <= 'Z') || (volume[0] >= 'a' && volume[0] <= 'z')) ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, "/") {
		return "", nil, errDirectories
	}
	root := volume + `\`
	if path == root {
		return root, nil, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, root), `\`)
	for _, part := range parts {
		if !safeDirectoryComponent(part) {
			return "", nil, errDirectories
		}
	}
	return root, parts, nil
}

// Retain the entire chain, not just the target, denying write/delete sharing
// to reject pre-existing mutation handles and prevent rename/reparse swaps.
// Private targets additionally deny read sharing, preventing SetSecurityInfo
// from propagating inheritable ACEs to unverified existing descendants.
// cache and tmp are secured explicitly; new descendants inherit the DACL.
// https://learn.microsoft.com/windows/win32/api/aclapi/nf-aclapi-setsecurityinfo
// There is no path-based security write after any component is opened.
type directoryHandles struct {
	handles []windows.Handle
}

func (d *directoryHandles) last() windows.Handle { return d.handles[len(d.handles)-1] }

func (d *directoryHandles) close() {
	for i := len(d.handles) - 1; i >= 0; i-- {
		if d.handles[i] != 0 {
			windows.CloseHandle(d.handles[i])
		}
	}
	d.handles = nil
}

func (d *directoryHandles) release(handle windows.Handle) {
	for i, owned := range d.handles {
		if owned == handle {
			windows.CloseHandle(handle)
			d.handles[i] = 0
			return
		}
	}
}

func openDirectoryHierarchy(path string) (_ *directoryHandles, err error) {
	root, parts, err := secureDirectoryPath(path)
	if err != nil {
		return nil, err
	}
	directories := &directoryHandles{}
	defer func() {
		if err != nil {
			directories.close()
		}
	}()
	// Only the volume root is absolute. Every filesystem component below it
	// is opened individually relative to an already verified, pinned handle.
	handle, err := directories.open(0, `\??\`+root, false, nil)
	if err != nil {
		return nil, err
	}
	if err = verifyDirectoryVolumeRoot(handle); err != nil {
		return nil, err
	}
	for _, part := range parts {
		handle, err = directories.openChild(handle, part, false, nil)
		if err != nil {
			return nil, err
		}
	}
	return directories, nil
}

func verifyDirectoryVolumeRoot(handle windows.Handle) error {
	// A drive letter can name a network share or a SUBST directory. Require
	// the opened object to be an actual local volume root, so no filesystem
	// ancestors are hidden in a DOS-device alias and left unpinned.
	const volumeNameGUID = 0x1
	var name [64]uint16 // A volume GUID root is 49 characters; longer is unsafe.
	n, err := windows.GetFinalPathNameByHandle(handle, &name[0], uint32(len(name)), volumeNameGUID)
	if err != nil {
		return err
	}
	if n == 0 || n >= uint32(len(name)) {
		return errDirectories
	}
	root := windows.UTF16ToString(name[:n])
	if !strings.HasPrefix(root, `\\?\Volume{`) || !strings.HasSuffix(root, `}\`) {
		return errDirectories
	}
	if _, err := windows.GUIDFromString(root[len(`\\?\Volume`) : len(root)-1]); err != nil {
		return errDirectories
	}
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return err
	}
	if flags&windows.FILE_PERSISTENT_ACLS == 0 {
		return errDirectories
	}
	return nil
}

func (d *directoryHandles) openChild(parent windows.Handle, name string, create bool, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	if parent == 0 || !safeDirectoryComponent(name) {
		return 0, errDirectories
	}
	return d.open(parent, name, create, descriptor)
}

func (d *directoryHandles) open(parent windows.Handle, name string, create bool, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	return d.openObject(parent, name, create, descriptor, true, false)
}

func (d *directoryHandles) openFile(parent windows.Handle, name string, create, write bool, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	if parent == 0 || !safeDirectoryComponent(name) {
		return 0, errDirectories
	}
	return d.openObject(parent, name, create, descriptor, false, write)
}

func (d *directoryHandles) openObject(parent windows.Handle, name string, create bool, descriptor *windows.SECURITY_DESCRIPTOR, directory, write bool) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent, ObjectName: objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	access := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	share := uint32(windows.FILE_SHARE_READ)
	if descriptor != nil {
		access |= windows.READ_CONTROL | windows.WRITE_DAC | windows.WRITE_OWNER
		share = 0 // Suppress automatic ACL propagation to unverified children.
	}
	disposition := uint32(windows.FILE_OPEN)
	if create {
		disposition = windows.FILE_OPEN_IF
	}
	options := uint32(windows.FILE_DIRECTORY_FILE)
	fileAttributes := uint32(windows.FILE_ATTRIBUTE_DIRECTORY)
	if !directory {
		options = windows.FILE_NON_DIRECTORY_FILE
		fileAttributes = windows.FILE_ATTRIBUTE_NORMAL
		access &^= windows.FILE_TRAVERSE
	}
	if write {
		access |= windows.FILE_WRITE_DATA
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, access, &attributes, &status, nil,
		fileAttributes, share, disposition,
		options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		if handle != 0 && handle != windows.InvalidHandle {
			windows.CloseHandle(handle)
		}
		return 0, err
	}
	// OPEN_REPARSE_POINT can return the reparse object itself. Inspect the
	// handle's attributes, never Lstat a name that can change under us.
	var info struct {
		Attributes uint32
		ReparseTag uint32
	}
	err = windows.GetFileInformationByHandleEx(handle, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil || (info.Attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory ||
		info.Attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.ReparseTag != 0 {
		windows.CloseHandle(handle)
		if err != nil {
			return 0, err
		}
		return 0, errDirectories
	}
	if !directory {
		var fileInfo windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &fileInfo); err != nil {
			windows.CloseHandle(handle)
			return 0, err
		}
		if fileInfo.NumberOfLinks != 1 {
			windows.CloseHandle(handle)
			return 0, errDirectories
		}
	}
	d.handles = append(d.handles, handle)
	return handle, nil
}
