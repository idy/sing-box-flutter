// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestStorageRejectsMediumFirstLaunchBeforeCreatingNamespace(t *testing.T) {
	base := directoryTestCacheRoot(t)
	working := filepath.Join(base, "New Host", "sing_box_flutter")
	token := mediumStorageToken(t)
	defer token.Close()
	if lease, err := initializeWindowsStorage(working, base, token); err == nil {
		lease.Close()
		t.Fatal("Medium first launch created managed storage")
	}
	if _, err := os.Lstat(filepath.Dir(working)); !os.IsNotExist(err) {
		t.Fatalf("failed authorization modified the storage namespace: %v", err)
	}
}

func TestStoragePrivateCreationPreventsLowerTokenMetadataAcquisition(t *testing.T) {
	base := directoryTestCacheRoot(t)
	working := filepath.Join(base, "New Host", "sing_box_flutter")
	openMetadata := func(path string, access uint32) (windows.Handle, error) {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return 0, err
		}
		return windows.CreateFile(name, access,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
			windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	}
	accesses := []uint32{windows.WRITE_DAC, windows.WRITE_OWNER, windows.WRITE_DAC | windows.WRITE_OWNER}
	withMediumIntegrity(t, func() {
		for _, access := range accesses {
			if handle, err := openMetadata(working, access); err == nil {
				windows.CloseHandle(handle)
				t.Fatalf("acquired metadata rights %#x before private creation", access)
			}
		}
	})
	lease, err := initializeStorage(working)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	state := filepath.Join(working, "cache", "native-state.bin")
	if err := os.WriteFile(state, []byte("persisted"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertStorageIntegrity(t, state, true)
	withMediumIntegrity(t, func() {
		for _, path := range []string{working, filepath.Join(working, "cache"), filepath.Join(working, "tmp"), state} {
			for _, access := range accesses {
				if handle, err := openMetadata(path, access); err == nil {
					windows.CloseHandle(handle)
					t.Fatalf("private creation allowed lower-token metadata rights %#x: %s", access, path)
				}
			}
		}
	})
}

func unsafeStorageDescriptor(t *testing.T, kind string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	dacl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + user.User.Sid.String() + ")"
	label, policy := "HI", "NWNR"
	switch kind {
	case "broad":
		dacl += "(A;OICI;FA;;;BU)"
	case "write-only":
		policy = "NW"
	default:
		label = "ME"
	}
	descriptor, err := windows.SecurityDescriptorFromString(dacl + "S:P(ML;OICI;" + policy + ";;;" + label + ")")
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func TestStorageRejectsUnsafeDescendantPolicyWithoutRepair(t *testing.T) {
	for _, kind := range []string{"broad", "medium"} {
		t.Run(kind, func(t *testing.T) {
			base := directoryTestCacheRoot(t)
			working := filepath.Join(base, "Host App", "sing_box_flutter")
			if err := initializeDirectories(working); err != nil {
				t.Fatal(err)
			}
			directories, err := openDirectoryHierarchy(working)
			if err != nil {
				t.Fatal(err)
			}
			defer directories.close()
			if _, err := directories.openFile(directories.last(), "preowned.bin", true, false, unsafeStorageDescriptor(t, kind)); err != nil {
				t.Fatal(err)
			}
			directories.close()
			path := filepath.Join(working, "preowned.bin")
			if err := os.WriteFile(path, []byte("old data"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := storagePolicySnapshot(t, path)
			if lease, err := initializeStorage(working); err == nil {
				lease.Close()
				t.Fatal("repaired/adopted an unsafe persisted object")
			}
			if after := storagePolicySnapshot(t, path); after != before {
				t.Fatalf("modified rejected persisted policy: %s -> %s", before, after)
			}
			if contents, err := os.ReadFile(path); err != nil || string(contents) != "old data" {
				t.Fatalf("modified rejected persisted contents: %q, %v", contents, err)
			}
		})
	}
}

func TestStorageRejectsExistingWriteOnlyRootAndDescendantsWithoutRepair(t *testing.T) {
	for _, name := range []string{".", "cache", "tmp", "core.lock", "tailscale-state", `tailscale-state\auth.bin`} {
		t.Run(name, func(t *testing.T) {
			base := directoryTestCacheRoot(t)
			working := filepath.Join(base, "Host App", "sing_box_flutter")
			if err := initializeDirectories(working); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(working, "tailscale-state", "auth.bin")
			if err := os.Mkdir(filepath.Dir(state), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(state, []byte("persisted secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(working, name)
			setOwnedCachePolicy(t, path, unsafeStorageDescriptor(t, "write-only"))
			before, stateBefore := storagePolicySnapshot(t, path), storagePolicySnapshot(t, state)
			if lease, err := initializeStorage(working); err == nil {
				lease.Close()
				t.Fatal("adopted legacy write-only policy that exposes same-user secrets")
			}
			if storagePolicySnapshot(t, path) != before || storagePolicySnapshot(t, state) != stateBefore {
				t.Fatal("repaired rejected write-only policy")
			}
			if data, err := os.ReadFile(state); err != nil || string(data) != "persisted secret" {
				t.Fatalf("rejected legacy state changed: %q, %v", data, err)
			}
		})
	}
}

func TestStorageRootBlocksHostParentLabelPropagation(t *testing.T) {
	base := directoryTestCacheRoot(t)
	host := filepath.Join(base, "Host App")
	if err := os.Mkdir(host, 0o700); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(host)
	if err != nil {
		t.Fatal(err)
	}
	openHost := func() (windows.Handle, error) {
		return windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_OWNER,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
			windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	}
	medium, err := windows.SecurityDescriptorFromString("S:(ML;OICI;NW;;;ME)")
	if err != nil {
		t.Fatal(err)
	}
	mediumACL, _, err := medium.SACL()
	if err != nil {
		t.Fatal(err)
	}
	setup, err := openHost()
	if err != nil {
		t.Fatal(err)
	}
	err = windows.SetSecurityInfo(setup, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, mediumACL)
	windows.CloseHandle(setup)
	if err != nil {
		t.Fatal(err)
	}
	var retained windows.Handle
	withMediumIntegrity(t, func() {
		var err error
		retained, err = openHost()
		if err != nil {
			t.Fatal(err)
		}
	})
	defer windows.CloseHandle(retained)
	before := storagePolicySnapshot(t, host)
	working := filepath.Join(host, "sing_box_flutter")
	lease, err := initializeStorage(working)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if after := storagePolicySnapshot(t, host); after != before {
		t.Fatal("SDK changed host-parent policy")
	}
	state := filepath.Join(working, "cache", "native.bin")
	if err := os.WriteFile(state, []byte("native state"), 0o600); err != nil {
		t.Fatal(err)
	}
	low, err := windows.SecurityDescriptorFromString("S:(ML;OICI;NW;;;LW)")
	if err != nil {
		t.Fatal(err)
	}
	lowACL, _, err := low.SACL()
	if err != nil {
		t.Fatal(err)
	}
	withMediumIntegrity(t, func() {
		if err := windows.SetSecurityInfo(retained, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, lowACL); err != nil {
			t.Fatalf("host-parent mutation fixture failed: %v", err)
		}
	})
	for _, path := range []string{working, filepath.Join(working, "cache"), state} {
		assertStorageIntegrity(t, path, false)
	}
}

func enableStorageCaseSensitivity(t *testing.T, path string) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	flags := uint32(windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR)
	err = windows.SetFileInformationByHandle(handle, windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)))
	if err == windows.ERROR_NOT_SUPPORTED || err == windows.ERROR_INVALID_PARAMETER || err == windows.ERROR_INVALID_FUNCTION {
		t.Skipf("per-directory case sensitivity unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestStorageVerifiesEveryCaseDistinctReservedName(t *testing.T) {
	for _, name := range []string{"CORE.LOCK", "Cache", "Tmp"} {
		t.Run(name, func(t *testing.T) {
			base := directoryTestCacheRoot(t)
			working := filepath.Join(base, "Host App", "sing_box_flutter")
			if err := os.MkdirAll(filepath.Dir(working), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := privateDirectory(working); err != nil {
				t.Fatal(err)
			}
			enableStorageCaseSensitivity(t, working)
			if err := initializeDirectories(working); err != nil {
				t.Fatal(err)
			}
			directories, err := openDirectoryHierarchy(working)
			if err != nil {
				t.Fatal(err)
			}
			defer directories.close()
			parent := directories.last()
			if sensitive, err := directoryCaseSensitive(parent); err != nil || !sensitive {
				t.Fatalf("case-sensitive fixture not enabled: %v, %v", sensitive, err)
			}
			bad := unsafeStorageDescriptor(t, "medium")
			var unsafeObject, original windows.Handle
			private, err := privateDirectorySecurity()
			if err != nil {
				t.Fatal(err)
			}
			if name == "CORE.LOCK" {
				unsafeObject, err = directories.openFile(parent, name, true, false, bad)
				if err == nil {
					original, err = directories.openFile(parent, "core.lock", false, false, private)
				}
			} else {
				unsafeObject, err = directories.openChild(parent, name, true, bad)
				if err == nil {
					lower := "cache"
					if name == "Tmp" {
						lower = "tmp"
					}
					original, err = directories.openChild(parent, lower, false, private)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			badID, err := storageFileID(unsafeObject)
			if err != nil {
				t.Fatal(err)
			}
			originalID, err := storageFileID(original)
			if err != nil || originalID == badID {
				t.Fatalf("fixture did not produce case-distinct identities: %v", err)
			}
			directories.close()
			if lease, err := initializeStorage(working); err == nil {
				lease.Close()
				t.Fatalf("skipped unsafe case-distinct %s", name)
			}
		})
	}
}
