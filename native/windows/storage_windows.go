// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// One owner retains the full hierarchy, all managed directories and the
// handle-relative core.lock until every core/check operation has closed.
type windowsStorage struct{ directories *directoryHandles }

func (s *windowsStorage) Close() error {
	s.directories.close()
	return nil
}

func initializePlatformStorage(path, base string) (_ io.Closer, err error) {
	descriptor, err := privateDirectorySecurity()
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
	// The host's policy is not ours. Missing host storage inherits normally.
	host, err := directories.openChild(directories.last(), filepath.Base(filepath.Dir(path)), true, nil)
	if err != nil {
		return nil, err
	}
	working, err := directories.sealDirectory(host, filepath.Base(path), true, descriptor)
	if err != nil {
		return nil, err
	}
	// FILE_OPEN_IF never truncates an existing lease. openFile verifies its
	// handle is a regular, single-link file before any ACL or label mutation.
	lock, err := directories.openFile(working, "core.lock", true, true, descriptor)
	if err != nil {
		return nil, err
	}
	if err = setPrivateDirectorySecurity(lock, descriptor); err != nil {
		return nil, err
	}
	children := make(map[string]windows.Handle)
	for _, name := range []string{"cache", "tmp"} {
		child, err := directories.sealDirectory(working, name, true, descriptor)
		if err != nil {
			return nil, err
		}
		children[name] = child
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
			if !safeDirectoryComponent(entry.Name()) {
				return errDirectories
			}
			if parent == working {
				if strings.EqualFold(entry.Name(), "core.lock") {
					continue // Already exclusively owned and verified above.
				}
				if child, ok := children[strings.ToLower(entry.Name())]; ok {
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
			if entry.IsDir() {
				child, err := directories.sealDirectory(parent, entry.Name(), false, descriptor)
				if err != nil {
					return err
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			} else {
				file, err := directories.openFile(parent, entry.Name(), false, false, descriptor)
				if err != nil {
					return err
				}
				if err := setPrivateDirectorySecurity(file, descriptor); err != nil {
					return err
				}
				// Native services must be able to reopen regular files for IO.
				// The sealed parent and file now deny lower-integrity mutation.
				directories.release(file)
			}
		}
		return nil
	}
	if err = walk(working, 0); err != nil {
		return nil, err
	}
	return &windowsStorage{directories: directories}, nil
}

func (d *directoryHandles) sealDirectory(parent windows.Handle, name string, create bool, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	exclusive, err := d.openChild(parent, name, create, descriptor)
	if err != nil {
		return 0, err
	}
	// Seal before enumerating: no lower-integrity process can insert new
	// children after this transition, including through an unvisited subtree.
	// Exclusive sharing suppresses implicit ACL/label propagation to children.
	if err := setPrivateDirectorySecurity(exclusive, descriptor); err != nil {
		return 0, err
	}
	var before windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(exclusive, &before); err != nil {
		return 0, err
	}
	d.release(exclusive)
	// Reopen only relative to the pinned parent, without following reparses.
	// Read sharing permits normal native directory enumeration. Write/delete
	// sharing stays denied for the helper lifetime. Identity must be unchanged.
	handle, err := d.openChild(parent, name, false, nil)
	if err != nil {
		return 0, err
	}
	var after windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &after); err != nil {
		return 0, err
	}
	if before.VolumeSerialNumber != after.VolumeSerialNumber || before.FileIndexHigh != after.FileIndexHigh || before.FileIndexLow != after.FileIndexLow {
		return 0, errDirectories
	}
	return handle, nil
}

func storageDirectoryEntries(handle windows.Handle) ([]os.DirEntry, error) {
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(windows.CurrentProcess(), handle, windows.CurrentProcess(),
		&duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), "verified storage directory")
	defer file.Close()
	// Enumerate the handle, never its pathname. Bound memory for hostile state.
	entries, err := file.ReadDir(16388)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) == 16388 {
		return nil, errDirectories
	}
	return entries, nil
}
