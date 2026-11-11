// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
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
	// Keep user+SYSTEM full control. Inheritable no-write-up and no-read-up at
	// the process integrity level (high for the elevated helper) prevent a lower
	// integrity process with the same user SID from reading auth/state secrets
	// or changing managed state. This policy is local to these objects.
	return windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + user.User.Sid.String() + ")S:P(ML;OICI;NWNR;;;" + label + ")")
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
	handles []windows.Handle
}

type nativeFileIdentity struct {
	volume string
	id     uint64
}

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
	windows.CloseHandle(handle)
}

// Duplicate the same file objects, including their real data-read sharing
// reservations. The controller and each core/check can then close independently.
func (d *directoryHandles) duplicate() (_ *directoryHandles, err error) {
	copy := &directoryHandles{}
	defer func() {
		if err != nil {
			copy.close()
		}
	}()
	for _, handle := range d.handles {
		if handle == 0 {
			continue
		}
		var duplicate windows.Handle
		if err := windows.DuplicateHandle(windows.CurrentProcess(), handle, windows.CurrentProcess(),
			&duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return nil, err
		}
		copy.handles = append(copy.handles, duplicate)
	}
	return copy, nil
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

// Only identities kept alive by this storage owner are known to be fresh.
// Even a managed file may have been imported/hardened after a Medium caller
// acquired metadata rights. Recreate every unknown identity before native IO;
// labels, ancestry and sharing probes are never used as historical provenance.
func (d *directoryHandles) pinNativeFile(parent windows.Handle, name string, descriptor *windows.SECURITY_DESCRIPTOR,
	known map[nativeFileIdentity]struct{}) (windows.Handle, error) {
	if parent == 0 || !safeDirectoryComponent(name) || descriptor == nil || len(d.handles) == 0 {
		return 0, errDirectories
	}
	candidate, err := d.openObject(parent, name, false, descriptor, storageObjectOptions{nativeIO: true})
	if err != nil && err != windows.STATUS_OBJECT_NAME_NOT_FOUND {
		return 0, err
	}
	var snapshot windows.Handle
	if candidate != 0 {
		identity, err := d.nativeFileIdentity(candidate)
		if err != nil {
			return 0, err
		}
		if _, ok := known[identity]; ok {
			return candidate, nil // A live owner keeper prevents deletion/ID reuse.
		}
		// Real DATA_READ with no WRITE sharing rejects already-granted writers
		// and blocks new ones during the entire copy. DELETE sharing permits
		// retiring this object while old metadata/read handles still refer to it.
		snapshot, err = d.openObject(parent, name, false, descriptor, storageObjectOptions{nativeIO: true, snapshot: true})
		if err != nil {
			return 0, err
		}
		snapshotIdentity, err := d.nativeFileIdentity(snapshot)
		if err != nil || snapshotIdentity != identity {
			return 0, errDirectories
		}
		d.release(candidate)
		defer d.release(snapshot)
	}
	return d.publishNativeFile(parent, name, snapshot, descriptor)
}

// Creation, copying and publication all use handles below the verified parent.
// The old object is never relabeled or given a new ACL. On a sharing conflict or
// unsupported atomic replacement, fail closed with the old contents untouched.
func (d *directoryHandles) publishNativeFile(parent windows.Handle, name string, snapshot windows.Handle,
	descriptor *windows.SECURITY_DESCRIPTOR) (_ windows.Handle, err error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return 0, err
	}
	temporary := ".cache-" + hex.EncodeToString(random[:]) + ".tmp"
	staging, err := d.openObject(parent, temporary, true, descriptor, storageObjectOptions{staging: true, fresh: true})
	if err != nil {
		return 0, err
	}
	published := false
	defer func() {
		if staging == 0 {
			return
		}
		if !published {
			// Only this freshly-created object is eligible for cleanup, by handle.
			var remove byte = 1
			_ = windows.SetFileInformationByHandle(staging, windows.FileDispositionInfo, &remove, 1)
		}
		d.release(staging)
	}()
	if snapshot != 0 {
		if err := copyStorageSnapshot(snapshot, staging); err != nil {
			return 0, err
		}
	}
	if err := windows.FlushFileBuffers(staging); err != nil {
		return 0, err
	}
	freshIdentity, err := d.nativeFileIdentity(staging)
	if err != nil {
		return 0, err
	}
	// ReOpenFile creates an independent file object without resolving a path.
	// A duplicated handle would retain the original file object's DELETE share
	// reservation. Keep this writer alive throughout the final no-delete open.
	writer, err := reopenStorageWriter(staging)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(writer)
	writerIdentity, err := d.nativeFileIdentity(writer)
	if err != nil || writerIdentity != freshIdentity {
		return 0, errDirectories
	}
	if err := renameStorageFile(staging, parent, name, snapshot != 0); err != nil {
		return 0, err
	}
	published = true
	d.release(staging) // Drop DELETE access before acquiring a no-delete pin.
	staging = 0        // The next open may reuse its numeric HANDLE value.
	return d.pinFreshNativeFile(parent, name, descriptor, freshIdentity)
}

// The independent writer must stay alive until this no-delete DATA_READ pin is
// obtained and checked. It prevents file-ID reuse if the path is substituted.
func (d *directoryHandles) pinFreshNativeFile(parent windows.Handle, name string, descriptor *windows.SECURITY_DESCRIPTOR,
	freshIdentity nativeFileIdentity) (windows.Handle, error) {
	pin, err := d.openObject(parent, name, false, descriptor, storageObjectOptions{nativeIO: true})
	if err != nil {
		return 0, err
	}
	identity, err := d.nativeFileIdentity(pin)
	if err != nil || identity != freshIdentity {
		return 0, errDirectories
	}
	// The writer kept freshIdentity allocated across the handoff; the real
	// DATA_READ pin now excludes DELETE and remains held before writer closes.
	return pin, nil
}

func copyStorageSnapshot(source, destination windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(source, &info); err != nil {
		return err
	}
	remaining := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	buffer := make([]byte, 64*1024)
	for remaining != 0 {
		size := uint64(len(buffer))
		if size > remaining {
			size = remaining
		}
		var read uint32
		if err := windows.ReadFile(source, buffer[:size], &read, nil); err != nil {
			return err
		}
		if read == 0 {
			return io.ErrUnexpectedEOF
		}
		var written uint32
		if err := windows.WriteFile(destination, buffer[:read], &written, nil); err != nil {
			return err
		}
		if written != read {
			return io.ErrShortWrite
		}
		remaining -= uint64(read)
	}
	return nil
}

var storageReOpenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

func reopenStorageWriter(handle windows.Handle) (windows.Handle, error) {
	if err := storageReOpenFile.Find(); err != nil {
		return 0, err
	}
	access := uint32(windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE)
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	value, _, err := storageReOpenFile.Call(uintptr(handle), uintptr(access), uintptr(share), 0)
	if windows.Handle(value) == windows.InvalidHandle {
		return 0, err
	}
	return windows.Handle(value), nil
}

// FILE_RENAME_INFORMATION_EX uses an ULONG flags field and natural HANDLE
// alignment (both supported architectures are 64-bit). Use Nt's relative target
// directory directly; do not reopen a pathname with directory mutation access.
type storageRenameInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

func renameStorageFile(source, parent windows.Handle, name string, replace bool) error {
	if parent == 0 || !safeDirectoryComponent(name) {
		return errDirectories
	}
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	length := uint32((len(encoded) - 1) * 2)
	offset := uint32(unsafe.Offsetof(storageRenameInformation{}.FileName))
	// Explicit 64-bit alignment is required by the native rename structure.
	words := make([]uint64, (offset+length+7)/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)
	info := (*storageRenameInformation)(unsafe.Pointer(&buffer[0]))
	info.RootDirectory, info.FileNameLength = parent, length
	if replace {
		info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	}
	copy(unsafe.Slice(&info.FileName[0], len(encoded)-1), encoded[:len(encoded)-1])
	var status windows.IO_STATUS_BLOCK
	const fileRenameInformationEx = 65
	return windows.NtSetInformationFile(source, &status, &buffer[0], offset+length, fileRenameInformationEx)
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
	snapshot, staging, fresh                        bool
}

func (d *directoryHandles) openObject(parent windows.Handle, name string, create bool, descriptor *windows.SECURITY_DESCRIPTOR, mode storageObjectOptions) (windows.Handle, error) {
	if mode.fresh && (!create || descriptor == nil) {
		return 0, errDirectories
	}
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
		// DATA_READ is essential: metadata-only opens do not establish the
		// read/delete sharing reservations used for snapshot and keeper safety.
		access = windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE
		share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	}
	if mode.snapshot {
		share = windows.FILE_SHARE_READ | windows.FILE_SHARE_DELETE
	}
	if mode.staging {
		access = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.SYNCHRONIZE
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
	if create && !mode.fresh && err == windows.STATUS_OBJECT_NAME_COLLISION {
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
