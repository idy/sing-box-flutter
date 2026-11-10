// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestStorageLeasePinsHierarchyAndPersistedStateUntilClose(t *testing.T) {
	base := directoryTestCacheRoot(t)
	working := filepath.Join(base, " Leading Host", "sing_box_flutter")
	nested := filepath.Join(working, "cache", "persisted")
	if err := os.MkdirAll(nested, 0o777); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(nested, "state.bin")
	if err := os.WriteFile(state, []byte("persisted state"), 0o666); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	broad, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;" + user.User.Sid.String() + ")(A;;GRGW;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := broad.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(state, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	hostBefore := directoryDACL(t, filepath.Dir(working))
	lease, err := initializeStorage(working)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	for _, path := range []string{base, filepath.Dir(working), working, filepath.Join(working, "cache"), filepath.Join(working, "tmp"), nested, filepath.Join(working, "core.lock")} {
		if err := os.Rename(path, path+"-moved"); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("lease did not pin storage object %s: %v", path, err)
		}
		handle, err := directoryMutationHandle(path)
		if err == nil {
			windows.CloseHandle(handle)
			t.Fatalf("lease admitted write/delete access to %s", path)
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("expected mutation sharing violation for %s: %v", path, err)
		}
	}
	for _, path := range []string{working, filepath.Join(working, "cache"), filepath.Join(working, "tmp"), nested, state, filepath.Join(working, "core.lock")} {
		assertPrivateStorageACL(t, path, true, false)
	}
	if after := directoryDACL(t, filepath.Dir(working)); after != hostBefore {
		t.Fatalf("host policy changed: %s -> %s", hostBefore, after)
	}
	if second, err := initializeStorage(working); err == nil {
		second.Close()
		t.Fatal("acquired a second storage owner")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(state); err != nil || string(contents) != "persisted state" {
		t.Fatalf("persisted state changed: %q, %v", contents, err)
	}
	if err := os.Rename(working, working+"-moved"); err != nil {
		t.Fatalf("close retained storage pins: %v", err)
	}
	if err := os.Rename(working+"-moved", working); err != nil {
		t.Fatal(err)
	}
	retry, err := initializeStorage(working)
	if err != nil {
		t.Fatalf("could not reopen safe persisted state: %v", err)
	}
	retry.Close()
}

func withMediumIntegrity(t *testing.T, operation func()) {
	t.Helper()
	if !isElevated() {
		t.Skip("lower-integrity isolation requires an elevated Windows test process")
	}
	var source, token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &source); err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := windows.DuplicateTokenEx(source, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE|windows.TOKEN_ADJUST_DEFAULT,
		nil, windows.SecurityImpersonation, windows.TokenImpersonation, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	sid, err := windows.CreateWellKnownSid(windows.WinMediumLabelSid)
	if err != nil {
		t.Fatal(err)
	}
	label := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: sid, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(token, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&label)), label.Size()); err != nil {
		t.Fatal(err)
	}
	runtime.LockOSThread()
	if err := windows.SetThreadToken(nil, token); err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	defer func() {
		if err := windows.RevertToSelf(); err != nil {
			panic(err)
		}
		runtime.UnlockOSThread()
	}()
	operation()
}

func assertStorageIntegrity(t *testing.T, path string, inherited bool) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.SACL()
	if err != nil || acl == nil || acl.AceCount != 1 {
		t.Fatalf("missing mandatory integrity label: %s, %v", descriptor.String(), err)
	}
	expected, err := privateDirectorySecurity()
	if err != nil {
		t.Fatal(err)
	}
	wantACL, _, err := expected.SACL()
	if err != nil {
		t.Fatal(err)
	}
	var got, want *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &got); err != nil {
		t.Fatal(err)
	}
	if err := windows.GetAce(wantACL, 0, &want); err != nil {
		t.Fatal(err)
	}
	if got.Header.AceType != 0x11 || got.Mask != want.Mask ||
		!windows.EqualSid((*windows.SID)(unsafe.Pointer(&got.SidStart)), (*windows.SID)(unsafe.Pointer(&want.SidStart))) ||
		inherited && got.Header.AceFlags&windows.INHERITED_ACE == 0 {
		t.Fatalf("incorrect/inherited mandatory label: %s", descriptor.String())
	}
}

func TestStorageIntegrityBlocksLowerIntegrityMutationAndPreservesNativeIO(t *testing.T) {
	if !isElevated() {
		t.Skip("requires elevated Windows helper")
	}
	base := directoryTestCacheRoot(t)
	working := filepath.Join(base, "Host App", "sing_box_flutter")
	lease, err := initializeStorage(working)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	state := filepath.Join(working, "cache", "profile.db")
	temp := filepath.Join(working, "tmp", "native-temp.bin")
	for _, path := range []string{state, temp} {
		if err := os.WriteFile(path, []byte("native state"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertStorageIntegrity(t, path, true)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "target.bin")
	if err := os.WriteFile(target, []byte("outside state"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := directoryDACL(t, target)
	withMediumIntegrity(t, func() {
		for _, path := range []string{working, filepath.Join(working, "cache"), filepath.Join(working, "tmp"), state, temp} {
			if err := os.Rename(path, path+"-moved"); err == nil {
				t.Fatalf("lower-integrity process renamed %s", path)
			}
			if handle, err := directoryMutationHandle(path); err == nil {
				windows.CloseHandle(handle)
				t.Fatalf("lower-integrity process acquired reparse mutation access to %s", path)
			}
		}
		if err := os.WriteFile(state, []byte("attacker"), 0o600); err == nil {
			t.Fatal("lower-integrity process wrote native state")
		}
		if err := os.Link(target, filepath.Join(working, "cache.db")); err == nil {
			t.Fatal("lower-integrity process inserted a hardlink")
		}
		if err := os.Mkdir(filepath.Join(working, "late-junction"), 0o700); err == nil {
			t.Fatal("lower-integrity process inserted an unverified directory")
		}
	})
	for _, path := range []string{state, temp} {
		if contents, err := os.ReadFile(path); err != nil || string(contents) != "native state" {
			t.Fatalf("native state changed: %q, %v", contents, err)
		}
	}
	if after := directoryDACL(t, target); after != before {
		t.Fatalf("outside DACL changed: %s -> %s", before, after)
	}
}

func TestStorageIntegrityResistsPreexistingMetadataHandles(t *testing.T) {
	if !isElevated() {
		t.Skip("requires elevated Windows helper")
	}
	base := directoryTestCacheRoot(t)
	working := filepath.Join(base, "Host App", "sing_box_flutter")
	if err := os.MkdirAll(working, 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(working, "persisted.bin")
	if err := os.WriteFile(state, []byte("persisted"), 0o600); err != nil {
		t.Fatal(err)
	}
	medium, err := windows.SecurityDescriptorFromString("S:(ML;OICI;NW;;;ME)")
	if err != nil {
		t.Fatal(err)
	}
	mediumACL, _, err := medium.SACL()
	if err != nil {
		t.Fatal(err)
	}
	openMetadata := func(path string) (windows.Handle, error) {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return 0, err
		}
		return windows.CreateFile(name, windows.WRITE_DAC|windows.WRITE_OWNER|windows.READ_CONTROL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	}
	paths := []string{working, state}
	// Only these owned fixtures start at medium integrity. No process/global
	// policy changes: the duplicated test token only impersonates this thread.
	for _, path := range paths {
		handle, err := openMetadata(path)
		if err != nil {
			t.Fatal(err)
		}
		err = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, mediumACL)
		windows.CloseHandle(handle)
		if err != nil {
			t.Fatal(err)
		}
	}
	var metadata []windows.Handle
	t.Cleanup(func() {
		for _, handle := range metadata {
			windows.CloseHandle(handle)
		}
	})
	withMediumIntegrity(t, func() {
		for _, path := range paths {
			handle, err := openMetadata(path)
			if err != nil {
				t.Fatal(err)
			}
			metadata = append(metadata, handle)
		}
	})
	lease, err := initializeStorage(working)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	private, err := privateDirectorySecurity()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := private.DACL()
	if err != nil {
		t.Fatal(err)
	}
	withMediumIntegrity(t, func() {
		for i, handle := range metadata {
			// WRITE_OWNER was granted before sealing and is not revoked by
			// file sharing. Relabel must still enforce the current integrity.
			_ = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, mediumACL)
			assertStorageIntegrity(t, paths[i], false)
			_ = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, nil)
			assertStorageIntegrity(t, paths[i], false)
			// Pre-granted WRITE_DAC may affect the DACL, but cannot bypass MIC.
			_ = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
		}
		if err := os.WriteFile(state, []byte("attacker"), 0o600); err == nil {
			t.Fatal("preexisting metadata rights re-enabled unsafe file writes")
		}
		if err := os.Mkdir(filepath.Join(working, "inserted"), 0o700); err == nil {
			t.Fatal("preexisting metadata rights re-enabled unsafe child insertion")
		}
	})
	if contents, err := os.ReadFile(state); err != nil || string(contents) != "persisted" {
		t.Fatalf("metadata attack changed persisted contents: %q, %v", contents, err)
	}
}

func TestStorageRejectsUnsafeLeaseAndDescendantsWithoutOutsideACLWrites(t *testing.T) {
	for _, kind := range []string{"junction", "symlink", "hardlink"} {
		for _, position := range []string{"lease", "descendant"} {
			if kind == "junction" && position == "lease" {
				continue // core.lock must be a regular file; covered by the other kinds.
			}
			t.Run(kind+"/"+position, func(t *testing.T) {
				base := directoryTestCacheRoot(t)
				working := filepath.Join(base, "Host App", "sing_box_flutter")
				nested := filepath.Join(working, "cache", "nested")
				if err := os.MkdirAll(nested, 0o700); err != nil {
					t.Fatal(err)
				}
				outside := t.TempDir()
				target := filepath.Join(outside, "target.bin")
				if err := os.WriteFile(target, []byte("outside state"), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(nested, "unsafe")
				if position == "lease" {
					link = filepath.Join(working, "core.lock")
				}
				switch kind {
				case "junction":
					createDirectoryJunction(t, link, outside)
				case "symlink":
					if err := os.Symlink(target, link); err != nil {
						t.Skipf("file symlinks unavailable: %v", err)
					}
				case "hardlink":
					if err := os.Link(target, link); err != nil {
						t.Fatal(err)
					}
				}
				before, directoryBefore := directoryDACL(t, target), directoryDACL(t, outside)
				if lease, err := initializeStorage(working); err == nil {
					lease.Close()
					t.Fatal("accepted unsafe preexisting storage")
				}
				if after := directoryDACL(t, target); after != before {
					t.Fatalf("outside file ACL changed: %s -> %s", before, after)
				}
				if after := directoryDACL(t, outside); after != directoryBefore {
					t.Fatalf("outside directory ACL changed: %s -> %s", directoryBefore, after)
				}
				if contents, err := os.ReadFile(target); err != nil || string(contents) != "outside state" {
					t.Fatalf("outside file was modified: %q, %v", contents, err)
				}
				if err := os.Rename(base, base+"-moved"); err != nil {
					t.Fatalf("failure retained a storage/ancestor pin: %v", err)
				}
				if err := os.Rename(base+"-moved", base); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
