// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func directoryDACL(t *testing.T, path string) string {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor.String()
}

func directoryMutationHandle(path string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(name, windows.GENERIC_WRITE|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// All reparse targets in these tests are owned temporary directories. A mount
// point junction exercises reparse rejection without requiring symlink privilege.
func directoryJunctionData(t *testing.T, target string) []byte {
	t.Helper()
	substitute, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		t.Fatal(err)
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 16+2*(len(substitute)+len(printName)))
	binary.LittleEndian.PutUint32(data, windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(data[4:], uint16(len(data)-8))
	binary.LittleEndian.PutUint16(data[10:], uint16(2*(len(substitute)-1)))
	binary.LittleEndian.PutUint16(data[12:], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(data[14:], uint16(2*(len(printName)-1)))
	for i, value := range append(substitute, printName...) {
		binary.LittleEndian.PutUint16(data[16+2*i:], value)
	}
	return data
}

func createDirectoryJunction(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	handle, err := directoryMutationHandle(path)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	data := directoryJunctionData(t, target)
	var returned uint32
	if err := windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT,
		&data[0], uint32(len(data)), nil, 0, &returned, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSecureDirectoriesRejectReparseWithoutChangingTargetACL(t *testing.T) {
	for _, kind := range []string{"junction", "symlink"} {
		for _, position := range []string{"ancestor", "base", "host", "working", "cache", "tmp"} {
			t.Run(kind+"/"+position, func(t *testing.T) {
				base := directoryTestCacheRoot(t)
				outside := t.TempDir()
				child := filepath.Join(outside, "existing-child")
				if err := os.Mkdir(child, 0o700); err != nil {
					t.Fatal(err)
				}
				before, childBefore := directoryDACL(t, outside), directoryDACL(t, child)
				host := filepath.Join(base, "Host App")
				working := filepath.Join(host, "sing_box_flutter")
				var link string
				switch position {
				case "ancestor", "base":
					link = filepath.Join(base, "Linked Cache")
					cacheRoot := link
					if position == "ancestor" {
						cacheRoot = filepath.Join(link, "existing-child")
					}
					t.Setenv("LOCALAPPDATA", cacheRoot)
					working = filepath.Join(cacheRoot, "Host App", "sing_box_flutter")
				case "host":
					link = host
				case "working":
					if err := os.Mkdir(host, 0o700); err != nil {
						t.Fatal(err)
					}
					link = working
				case "cache", "tmp":
					if err := os.MkdirAll(working, 0o700); err != nil {
						t.Fatal(err)
					}
					link = filepath.Join(working, position)
				}
				if kind == "junction" {
					createDirectoryJunction(t, link, outside)
				} else if err := os.Symlink(outside, link); err != nil {
					t.Skipf("directory symlinks unavailable: %v", err)
				}
				if initializeDirectories(working) == nil {
					t.Fatal("accepted a reparse point")
				}
				if privateDirectory(filepath.Join(link, "private-child")) == nil {
					t.Fatal("privateDirectory accepted a reparse ancestor")
				}
				if privateDirectory(link) == nil {
					t.Fatal("privateDirectory accepted a reparse target")
				}
				if after := directoryDACL(t, outside); after != before {
					t.Fatalf("outside target DACL changed: %s -> %s", before, after)
				}
				if after := directoryDACL(t, child); after != childBefore {
					t.Fatalf("outside child DACL changed: %s -> %s", childBefore, after)
				}
				entries, err := os.ReadDir(outside)
				if err != nil || len(entries) != 1 || entries[0].Name() != "existing-child" {
					t.Fatalf("created directories outside storage: %v, %v", entries, err)
				}
				// Failure must release even the ancestor handles acquired before
				// encountering the reparse point.
				moved := link + "-moved"
				if err := os.Rename(link, moved); err != nil {
					t.Fatalf("failure retained a directory handle: %v", err)
				}
				if err := os.Rename(base, base+"-moved"); err != nil {
					t.Fatalf("failure retained an ancestor handle: %v", err)
				}
				if err := os.Rename(base+"-moved", base); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSecureDirectoryHandlesBlockMutationDuringACLWrite(t *testing.T) {
	base := directoryTestCacheRoot(t)
	descriptor, err := privateDirectorySecurity()
	if err != nil {
		t.Fatal(err)
	}
	directories, err := openDirectoryHierarchy(base)
	if err != nil {
		t.Fatal(err)
	}
	defer directories.close()
	hostPath := filepath.Join(base, "Host App")
	host, err := directories.openChild(directories.last(), "Host App", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	workingPath := filepath.Join(hostPath, "sing_box_flutter")
	working, err := directories.openChild(host, "sing_box_flutter", true, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	before := directoryDACL(t, outside)
	data := directoryJunctionData(t, outside)
	// Deterministically attempt mutation while the same verified handles used
	// for creation and SetSecurityInfo remain open. No timing-dependent swap.
	for _, path := range []string{base, hostPath, workingPath} {
		if err := os.Rename(path, path+"-moved"); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("pinned directory rename should fail with sharing violation: %s, %v", path, err)
		}
		handle, err := directoryMutationHandle(path)
		if err == nil {
			// If a write handle is ever granted, try the actual junction update
			// against our safe temp target before reporting the broken pin.
			var returned uint32
			mutationErr := windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT,
				&data[0], uint32(len(data)), nil, 0, &returned, nil)
			windows.CloseHandle(handle)
			t.Fatalf("pinned directory admitted a mutation handle: %s, reparse update: %v", path, mutationErr)
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("reparse mutation should be blocked by sharing: %s, %v", path, err)
		}
		if err := setPrivateDirectorySecurity(working, descriptor); err != nil {
			t.Fatal(err)
		}
	}
	if after := directoryDACL(t, outside); after != before {
		t.Fatalf("outside DACL changed during mutation attempts: %s -> %s", before, after)
	}
	directories.close()
	for _, path := range []string{workingPath, hostPath, base} {
		handle, err := directoryMutationHandle(path)
		if err != nil {
			t.Fatalf("closed hierarchy retained a handle: %s, %v", path, err)
		}
		windows.CloseHandle(handle)
		if err := os.Rename(path, path+"-moved"); err != nil {
			t.Fatalf("closed hierarchy still prevents rename: %v", err)
		}
		if err := os.Rename(path+"-moved", path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSecureDirectoriesRejectExistingMutationHandles(t *testing.T) {
	for _, position := range []string{"base", "host", "working"} {
		t.Run(position, func(t *testing.T) {
			base := directoryTestCacheRoot(t)
			host := filepath.Join(base, "Host App")
			working := filepath.Join(host, "sing_box_flutter")
			paths := map[string]string{"base": base, "host": host, "working": working}
			path := paths[position]
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			before := directoryDACL(t, path)
			handle, err := directoryMutationHandle(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if handle != 0 {
					windows.CloseHandle(handle)
				}
			})
			if initializeDirectories(working) == nil {
				t.Fatal("accepted an ancestor/target with an existing mutation handle")
			}
			if after := directoryDACL(t, path); after != before {
				t.Fatalf("DACL changed despite an existing mutation handle: %s -> %s", before, after)
			}
			entries, err := os.ReadDir(path)
			if err != nil || len(entries) != 0 {
				t.Fatalf("created directories before pinning hierarchy: %v, %v", entries, err)
			}
			windows.CloseHandle(handle)
			handle = 0
			if err := initializeDirectories(working); err != nil {
				t.Fatalf("failure leaked a handle or prevented retry: %v", err)
			}
		})
	}
}

func TestPrivateDirectoryACLAndNoUnverifiedPropagation(t *testing.T) {
	base := directoryTestCacheRoot(t)
	path := filepath.Join(base, "private")
	if err := os.Mkdir(path, 0o777); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(path, "existing")
	if err := os.Mkdir(existing, 0o777); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	createDirectoryJunction(t, filepath.Join(path, "junction"), outside)
	existingBefore, outsideBefore := directoryDACL(t, existing), directoryDACL(t, outside)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := privateDirectory(path); err != nil {
			t.Fatal(err)
		}
		text := directoryDACL(t, path)
		if !strings.HasPrefix(text, "D:P") || strings.Count(text, "(A;") != 2 ||
			!strings.Contains(text, "(A;OICI;FA;;;SY)") ||
			!strings.Contains(text, "(A;OICI;FA;;;"+user.User.Sid.String()+")") {
			t.Fatalf("directory has an unexpected private DACL: %s", text)
		}
		if after := directoryDACL(t, existing); after != existingBefore {
			t.Fatalf("ACL write propagated to an unverified child: %s -> %s", existingBefore, after)
		}
		if after := directoryDACL(t, outside); after != outsideBefore {
			t.Fatalf("ACL write followed a descendant junction: %s -> %s", outsideBefore, after)
		}
	}
	// New children must still inherit full control for the user and SYSTEM.
	child := filepath.Join(path, "new-child")
	if err := os.Mkdir(child, 0o777); err != nil {
		t.Fatal(err)
	}
	if text := directoryDACL(t, child); !strings.Contains(text, ";FA;;;SY)") ||
		!strings.Contains(text, ";FA;;;"+user.User.Sid.String()+")") || strings.Contains(text, ";;;BU)") {
		t.Fatalf("new child did not inherit the private DACL: %s", text)
	}
	moved := path + "-moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("privateDirectory retained a handle after success: %v", err)
	}
}

func TestSecureDirectoriesRejectUnsafeWindowsPaths(t *testing.T) {
	for _, path := range []string{
		``, `relative`, `C:relative`, `\rooted`, `\\server\share\private`,
		`\\?\C:\private`, `\\.\C:\private`, `C:\host\..\private`,
		`C:\host.\private`, `C:\host \private`, `C:\host:stream\private`,
		`C:\CON\private`, `C:\NUL.txt\private`, `C:\host/private`, `C:\`,
	} {
		// Parse only: never try privileged operations on system/device paths.
		_, parts, err := secureDirectoryPath(path)
		if err == nil && len(parts) != 0 {
			t.Errorf("accepted unsafe private directory path %q", path)
		}
	}
}
