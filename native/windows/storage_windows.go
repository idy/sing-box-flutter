// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"io"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// One owner retains the verified hierarchy, all managed directory pins and
// handle-relative core.lock until every core/check operation has closed.
type windowsStorage struct{ directories *directoryHandles }

func (s *windowsStorage) Close() error {
	s.directories.close()
	return nil
}

func initializePlatformStorage(path, base string) (io.Closer, error) {
	return initializeWindowsStorage(path, base, windows.GetCurrentProcessToken())
}

func initializeWindowsStorage(path, base string, token windows.Token) (_ io.Closer, err error) {
	// Authorization precedes every filesystem operation, including creation
	// of the ordinary host parent. A first Medium launch cannot poison v1 state.
	descriptor, err := privateDirectorySecurityForToken(token)
	if err != nil {
		return nil, err
	}
	directories, err := openDirectoryHierarchy(base)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			directories.close()
		}
	}()
	// The host's existing security policy is never changed. Only missing
	// host storage is created, with ordinary inherited security.
	host, err := directories.openChild(directories.last(), filepath.Base(filepath.Dir(path)), true, nil)
	if err != nil {
		return nil, err
	}
	working, err := directories.openChild(host, filepath.Base(path), true, descriptor)
	if err != nil {
		return nil, err
	}
	if err = verifyPrivateRoot(working); err != nil {
		return nil, err
	}
	// FILE_CREATE supplies the full private policy atomically for new objects.
	// A collision is opened without mutation and must already match policy.
	lock, err := directories.openFile(working, "core.lock", true, true, descriptor)
	if err != nil {
		return nil, err
	}
	lockID, err := storageFileID(lock)
	if err != nil {
		return nil, err
	}
	known := make(map[uint64]windows.Handle)
	for _, name := range []string{"cache", "tmp"} {
		child, err := directories.openChild(working, name, true, descriptor)
		if err != nil {
			return nil, err
		}
		id, err := storageFileID(child)
		if err != nil {
			return nil, err
		}
		known[id] = child
	}
	remaining := 16384
	var walk func(windows.Handle, int) error
	walk = func(parent windows.Handle, depth int) error {
		if depth > 64 {
			return errDirectories
		}
		entries, err := storageDirectoryEntries(parent)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !safeDirectoryComponent(entry.name) || entry.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return errDirectories
			}
			isDirectory := entry.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
			if parent == working {
				// Compare identities, never folded names. Case-sensitive Windows
				// directories can contain both core.lock/CORE.LOCK or cache/Cache.
				if entry.id == lockID {
					if isDirectory {
						return errDirectories
					}
					continue
				}
				if child, ok := known[entry.id]; ok {
					if !isDirectory {
						return errDirectories
					}
					if err := walk(child, depth+1); err != nil {
						return err
					}
					continue
				}
			}
			remaining--
			if remaining < 0 {
				return errDirectories
			}
			var child windows.Handle
			if isDirectory {
				child, err = directories.openChild(parent, entry.name, false, descriptor)
			} else {
				child, err = directories.openFile(parent, entry.name, false, false, descriptor)
			}
			if err != nil {
				return err
			}
			id, err := storageFileID(child)
			if err != nil || id != entry.id {
				return errDirectories
			}
			if isDirectory {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			} else {
				// Native IO may reopen safe regular files. The private policy was
				// present at creation; no old lower-token metadata handle can have
				// been acquired before an in-place privilege transition here.
				directories.release(child)
			}
		}
		return nil
	}
	if err = walk(working, 0); err != nil {
		return nil, err
	}
	return &windowsStorage{directories: directories}, nil
}

func verifyPrivateRoot(handle windows.Handle) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil || control&(windows.SE_DACL_PROTECTED|windows.SE_SACL_PROTECTED) != windows.SE_DACL_PROTECTED|windows.SE_SACL_PROTECTED {
		return errDirectories
	}
	label, _, err := descriptor.SACL()
	if err != nil || label == nil || label.AceCount != 1 {
		return errDirectories
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if windows.GetAce(label, 0, &ace) != nil || ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
		return errDirectories
	}
	return nil
}

func storageFileID(handle windows.Handle) (uint64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, err
	}
	id := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	if id == 0 {
		return 0, errDirectories
	}
	return id, nil
}

type storageEntry struct {
	name       string
	attributes uint32
	id         uint64
}

// FILE_ID_BOTH_DIR_INFO, used only for handle-based directory enumeration.
// The FileName field starts at offset 104 on the supported Windows targets.
type storageDirectoryInfo struct {
	NextEntryOffset, FileIndex                              uint32
	CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
	EndOfFile, AllocationSize                               int64
	FileAttributes, FileNameLength, EaSize                  uint32
	ShortNameLength                                         uint8
	ShortName                                               [12]uint16
	FileID                                                  uint64
	FileName                                                [1]uint16
}

func storageDirectoryEntries(handle windows.Handle) ([]storageEntry, error) {
	buffer := make([]byte, 64*1024)
	class := uint32(windows.FileIdBothDirectoryRestartInfo)
	var entries []storageEntry
	for {
		err := windows.GetFileInformationByHandleEx(handle, class, &buffer[0], uint32(len(buffer)))
		if err == windows.ERROR_NO_MORE_FILES || err == windows.ERROR_FILE_NOT_FOUND && class == windows.FileIdBothDirectoryRestartInfo {
			return entries, nil
		}
		if err != nil {
			return nil, err // Filesystems without stable directory IDs are unsafe.
		}
		class = windows.FileIdBothDirectoryInfo
		for offset := uint32(0); ; {
			const nameOffset = uint32(unsafe.Offsetof(storageDirectoryInfo{}.FileName))
			if offset > uint32(len(buffer))-nameOffset {
				return nil, errDirectories
			}
			info := (*storageDirectoryInfo)(unsafe.Pointer(&buffer[offset]))
			limit := uint32(len(buffer)) - offset
			if info.NextEntryOffset != 0 {
				if info.NextEntryOffset > limit || info.NextEntryOffset < nameOffset {
					return nil, errDirectories
				}
				limit = info.NextEntryOffset
			}
			if info.FileNameLength%2 != 0 || info.FileNameLength > limit-nameOffset {
				return nil, errDirectories
			}
			name := windows.UTF16ToString(unsafe.Slice(&info.FileName[0], info.FileNameLength/2))
			if name != "." && name != ".." {
				if len(entries) >= 16388 || info.FileID == 0 {
					return nil, errDirectories
				}
				entries = append(entries, storageEntry{name: name, attributes: info.FileAttributes, id: info.FileID})
			}
			if info.NextEntryOffset == 0 {
				break
			}
			offset += info.NextEntryOffset
		}
	}
}
