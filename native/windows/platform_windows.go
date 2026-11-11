// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"sync"
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
	_, err = directories.openChild(directories.last(), parts[len(parts)-1], true, descriptor)
	return err
}

func privateDirectorySecurity() (*windows.SECURITY_DESCRIPTOR, error) {
	return privateDirectorySecurityForToken(windows.GetCurrentProcessToken())
}

func privateDirectorySecurityForToken(token windows.Token) (*windows.SECURITY_DESCRIPTOR, error) {
	if !token.IsElevated() {
		return nil, errDirectories
	}
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	var size uint32
	err = windows.GetTokenInformation(token, windows.TokenIntegrityLevel, nil, 0, &size)
	if err != windows.ERROR_INSUFFICIENT_BUFFER || size < uint32(unsafe.Sizeof(windows.Tokenmandatorylabel{})) {
		if err != nil {
			return nil, err
		}
		return nil, errDirectories
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buffer[0], size, &size); err != nil {
		return nil, err
	}
	sid := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buffer[0])).Label.Sid
	if sid.IdentifierAuthority() != windows.SECURITY_MANDATORY_LABEL_AUTHORITY || sid.SubAuthorityCount() != 1 || sid.SubAuthority(0) < 0x3000 {
		return nil, errDirectories // Never create Medium state on a first launch.
	}
	label := sid.String()
	// Keep user+SYSTEM full control. An inheritable no-write-up label at the
	// process integrity level (high for the elevated helper) prevents a lower
	// integrity process with the same user SID from changing managed state.
	// This is local to these objects; no token privilege or global policy changes.
	return windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + user.User.Sid.String() + ")S:P(ML;OICI;NW;;;" + label + ")")
}

// Existing objects are never upgraded: a previously granted WRITE_OWNER or
// WRITE_DAC handle survives ACL/label changes and could undo an in-place seal.
// Only objects created with the required policy can be safely adopted here.
func verifyPrivateObject(handle windows.Handle, expected *windows.SECURITY_DESCRIPTOR, directory bool) error {
	actual, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := actual.DACL()
	if err != nil || dacl == nil {
		return errDirectories
	}
	wantDACL, _, err := expected.DACL()
	if err != nil || !samePrivateACL(dacl, wantDACL, directory, false) {
		return errDirectories
	}
	label, _, err := actual.SACL()
	if err != nil || label == nil {
		return errDirectories
	}
	wantLabel, _, err := expected.SACL()
	if err != nil || !samePrivateACL(label, wantLabel, directory, true) {
		return errDirectories
	}
	return nil
}

func samePrivateACL(actual, expected *windows.ACL, directory, label bool) bool {
	if actual == nil || expected == nil || actual.AceCount != expected.AceCount {
		return false
	}
	used := make([]bool, expected.AceCount)
	for i := uint32(0); i < uint32(actual.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(actual, i, &ace) != nil ||
			ace.Header.AceFlags & ^uint8(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE|windows.INHERITED_ACE) != 0 ||
			directory && ace.Header.AceFlags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
			return false
		}
		typeExpected := uint8(windows.ACCESS_ALLOWED_ACE_TYPE)
		if label {
			typeExpected = 0x11 // SYSTEM_MANDATORY_LABEL_ACE_TYPE
		}
		if ace.Header.AceType != typeExpected {
			return false
		}
		matched := false
		for j := uint32(0); j < uint32(expected.AceCount); j++ {
			var want *windows.ACCESS_ALLOWED_ACE
			if windows.GetAce(expected, j, &want) != nil {
				return false
			}
			if !used[j] && ace.Mask == want.Mask && windows.EqualSid((*windows.SID)(unsafe.Pointer(&ace.SidStart)), (*windows.SID)(unsafe.Pointer(&want.SidStart))) {
				used[j], matched = true, true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
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

// Retain the entire chain, denying write/delete sharing to prevent
// rename/reparse swaps. All private policy is supplied at FILE_CREATE;
// existing objects are verified without any security mutation.
type directoryHandles struct {
	handles     []windows.Handle
	nativeFiles map[windows.Handle]nativeFileIdentity
}

type nativeFileIdentity struct {
	volume string
	id     uint64
}

// Active admissions allow checks to reuse a file already pinned by this
// process while its trusted native database has data handles open. A count is
// removed before the final pin closes; no admission survives its owning leases.
var nativeFileAdmissions = struct {
	sync.Mutex
	files map[nativeFileIdentity]int
}{files: make(map[nativeFileIdentity]int)}

func (d *directoryHandles) last() windows.Handle { return d.handles[len(d.handles)-1] }

func (d *directoryHandles) close() {
	for i := len(d.handles) - 1; i >= 0; i-- {
		if d.handles[i] != 0 {
			d.closeHandle(d.handles[i])
		}
	}
	d.handles = nil
}

func (d *directoryHandles) release(handle windows.Handle) {
	for i, owned := range d.handles {
		if owned == handle {
			d.closeHandle(handle)
			d.handles[i] = 0
			return
		}
	}
}

func (d *directoryHandles) closeHandle(handle windows.Handle) {
	if identity, ok := d.nativeFiles[handle]; ok {
		nativeFileAdmissions.Lock()
		defer nativeFileAdmissions.Unlock()
		delete(d.nativeFiles, handle)
		if nativeFileAdmissions.files[identity] <= 1 {
			delete(nativeFileAdmissions.files, identity)
		} else {
			nativeFileAdmissions.files[identity]--
		}
	}
	windows.CloseHandle(handle)
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
	return d.openObject(parent, name, create, descriptor, storageObjectOptions{directory: true})
}

// Full SACL protection is a root invariant, separate from its label ACE.
func (d *directoryHandles) openPrivateRoot(parent windows.Handle, name string, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	if parent == 0 || !safeDirectoryComponent(name) || descriptor == nil {
		return 0, errDirectories
	}
	return d.openObject(parent, name, true, descriptor, storageObjectOptions{directory: true, readSACL: true})
}

func (d *directoryHandles) openFile(parent windows.Handle, name string, create, write bool, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	if parent == 0 || !safeDirectoryComponent(name) {
		return 0, errDirectories
	}
	return d.openObject(parent, name, create, descriptor, storageObjectOptions{write: write})
}

// Retain a metadata-only file pin that denies delete/rename but permits the
// native database to reopen it for data IO. The parent must already be private;
// both new and existing files must satisfy the same immutable private policy.
func (d *directoryHandles) pinNativeFile(parent windows.Handle, name string, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	if parent == 0 || !safeDirectoryComponent(name) || descriptor == nil || len(d.handles) == 0 {
		return 0, errDirectories
	}
	nativeFileAdmissions.Lock()
	defer nativeFileAdmissions.Unlock()
	// A current high label/DACL cannot revoke an old data handle granted
	// before an administrator hardened a file. share=0 detects retained
	// read/write/delete access, without requesting any data access itself.
	probe, err := d.openObject(parent, name, true, descriptor, storageObjectOptions{nativeIO: true, exclusive: true})
	if err != nil && err != windows.STATUS_SHARING_VIOLATION {
		return 0, err
	}
	if probe != 0 {
		defer d.release(probe)
	}
	pin, err := d.openObject(parent, name, false, descriptor, storageObjectOptions{nativeIO: true})
	if err != nil {
		return 0, err
	}
	identity, err := d.nativeFileIdentity(pin)
	if err != nil {
		return 0, err
	}
	if probe != 0 {
		probeIdentity, err := d.nativeFileIdentity(probe)
		if err != nil || probeIdentity != identity {
			return 0, errDirectories
		}
		// Both opens request metadata only, so they overlap even with share=0.
		// The final no-delete pin and private parent remain held when the probe
		// closes: there is no unpinned close/reopen gap for a path substitution.
	} else if nativeFileAdmissions.files[identity] == 0 {
		return 0, errDirectories // A data handle exists without an active admission.
	}
	if d.nativeFiles == nil {
		d.nativeFiles = make(map[windows.Handle]nativeFileIdentity)
	}
	d.nativeFiles[pin] = identity
	nativeFileAdmissions.files[identity]++
	return pin, nil
}

func (d *directoryHandles) nativeFileIdentity(handle windows.Handle) (nativeFileIdentity, error) {
	// Volume serial numbers can be duplicated on cloned volumes. Key active
	// admissions by the verified volume root's GUID and stable file ID instead.
	var volume [64]uint16
	n, err := windows.GetFinalPathNameByHandle(d.handles[0], &volume[0], uint32(len(volume)), 0x1)
	if err != nil || n == 0 || n >= uint32(len(volume)) {
		return nativeFileIdentity{}, errDirectories
	}
	id, err := storageFileID(handle)
	if err != nil {
		return nativeFileIdentity{}, err
	}
	return nativeFileIdentity{volume: windows.UTF16ToString(volume[:n]), id: id}, nil
}

type storageObjectOptions struct {
	directory, write, readSACL, nativeIO, exclusive bool
}

func (d *directoryHandles) openObject(parent windows.Handle, name string, create bool, descriptor *windows.SECURITY_DESCRIPTOR, mode storageObjectOptions) (windows.Handle, error) {
	directory, write, readSACL := mode.directory, mode.write, mode.readSACL
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	objectFlags := uint32(windows.OBJ_DONT_REPARSE)
	if parent == 0 {
		objectFlags |= windows.OBJ_CASE_INSENSITIVE // Only the volume root.
	} else {
		sensitive, err := directoryCaseSensitive(parent)
		if err != nil {
			return 0, err
		}
		if !sensitive {
			objectFlags |= windows.OBJ_CASE_INSENSITIVE
		}
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent, ObjectName: objectName,
		Attributes: objectFlags,
	}
	if create {
		attributes.SecurityDescriptor = descriptor
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	access := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	share := uint32(windows.FILE_SHARE_READ)
	if descriptor != nil {
		access |= windows.READ_CONTROL
	}
	if readSACL {
		access |= windows.ACCESS_SYSTEM_SECURITY
	}
	if write {
		share = 0 // Exclusive, handle-relative ownership lease.
	}
	disposition := uint32(windows.FILE_OPEN)
	if create {
		disposition = windows.FILE_CREATE
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
	if mode.nativeIO {
		access = windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE
		share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	}
	if mode.exclusive {
		share = 0
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	open := func() error {
		return windows.NtCreateFile(&handle, access, &attributes, &status, nil,
			fileAttributes, share, disposition,
			options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	}
	openWithAccess := func() error {
		if attributes.SecurityDescriptor != nil || readSACL {
			return withStorageSecurityPrivilege(open)
		}
		return open()
	}
	err = openWithAccess()
	if create && err == windows.STATUS_OBJECT_NAME_COLLISION {
		// Atomic creation did not win. Open the existing object without any
		// creation descriptor; only root SACL reads still need the privilege.
		disposition = windows.FILE_OPEN
		attributes.SecurityDescriptor = nil
		err = openWithAccess()
	}
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
	if descriptor != nil {
		if err := verifyPrivateObject(handle, descriptor, directory); err != nil {
			windows.CloseHandle(handle)
			return 0, err
		}
	}
	d.handles = append(d.handles, handle)
	return handle, nil
}

func directoryCaseSensitive(handle windows.Handle) (bool, error) {
	var flags uint32
	err := windows.GetFileInformationByHandleEx(handle, windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)))
	if err == windows.ERROR_INVALID_PARAMETER || err == windows.ERROR_NOT_SUPPORTED || err == windows.ERROR_INVALID_FUNCTION {
		return false, nil // Older Windows/filesystems cannot enable this flag.
	}
	if err != nil {
		return false, err
	}
	if flags & ^uint32(windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR) != 0 {
		return false, errDirectories
	}
	return flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0, nil
}
