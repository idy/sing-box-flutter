// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"bytes"
	"errors"
	"os"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func creationTokenID(t *testing.T, token windows.Token) []byte {
	t.Helper()
	var size uint32
	if err := windows.GetTokenInformation(token, windows.TokenStatistics, nil, 0, &size); err != windows.ERROR_INSUFFICIENT_BUFFER {
		t.Fatalf("query token statistics size: %v", err)
	}
	if size < 8 {
		t.Fatal("token statistics do not contain a token ID")
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenStatistics, &buffer[0], size, &size); err != nil {
		t.Fatal(err)
	}
	return buffer[:8] // TOKEN_STATISTICS starts with the LUID TokenId.
}

// Check privilege presence independently of the helper under test. Skipping on
// storageSecurityToken's result could hide a regression enabling a held privilege.
func requireCreationSecurityPrivilege(t *testing.T) {
	t.Helper()
	require := func(reason string) {
		if os.Getenv("SING_BOX_FLUTTER_WINTUN_SMOKE") == "1" {
			t.Fatal("explicit elevated smoke requires creation-security assertions: " + reason)
		}
		t.Skip(reason)
	}
	token := windows.GetCurrentProcessToken()
	if !token.IsElevated() {
		require("creation-security privilege assertions require an elevated token")
	}
	var luid windows.LUID
	name, _ := windows.UTF16PtrFromString("SeSecurityPrivilege")
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		t.Fatal(err)
	}
	var size uint32
	if err := windows.GetTokenInformation(token, windows.TokenPrivileges, nil, 0, &size); err != windows.ERROR_INSUFFICIENT_BUFFER {
		t.Fatalf("query held privileges: %v", err)
	}
	offset := uint32(unsafe.Offsetof(windows.Tokenprivileges{}.Privileges))
	if size < offset {
		t.Fatal("invalid privilege information size")
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenPrivileges, &buffer[0], size, &size); err != nil {
		t.Fatal(err)
	}
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&buffer[0]))
	if privileges.PrivilegeCount > (size-offset)/uint32(unsafe.Sizeof(windows.LUIDAndAttributes{})) {
		t.Fatal("invalid held privilege count")
	}
	for _, privilege := range privileges.AllPrivileges() {
		if privilege.Luid == luid {
			return // Disabled is still held and must be exercised by the test.
		}
	}
	require("SeSecurityPrivilege is absent from the process token")
}

func TestCreationSecurityPreservesCallerAndProcessOnSuccessAndFailure(t *testing.T) {
	requireCreationSecurityPrivilege(t)
	runtime.LockOSThread()
	var saved windows.Token
	err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE, true, &saved)
	if err != nil && err != windows.ERROR_NO_TOKEN {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	defer func() {
		if err := windows.SetThreadToken(nil, saved); err != nil {
			t.Errorf("restore test caller: %v", err)
			// Exit the test goroutine with this thread still locked.
		} else {
			runtime.UnlockOSThread()
		}
		if saved != 0 {
			saved.Close()
		}
	}()
	var source, outer windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &source); err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := windows.DuplicateTokenEx(source, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE|windows.TOKEN_ADJUST_PRIVILEGES,
		nil, windows.SecurityImpersonation, windows.TokenImpersonation, &outer); err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	if err := windows.SetThreadToken(nil, outer); err != nil {
		t.Fatal(err)
	}
	var luid windows.LUID
	name, _ := windows.UTF16PtrFromString("SeSecurityPrivilege")
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		t.Fatal(err)
	}
	before, err := tokenPrivilegeEnabled(source, luid)
	if err != nil {
		t.Fatal(err)
	}
	outerID := creationTokenID(t, outer)
	// Force a disabled caller privilege even when the process already enabled
	// it, so enabling only the worker duplicate is an observable requirement.
	disabled := windows.Tokenprivileges{PrivilegeCount: 1,
		Privileges: [1]windows.LUIDAndAttributes{{Luid: luid}}}
	if err := windows.AdjustTokenPrivileges(outer, false, &disabled, 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	outerBefore, err := tokenPrivilegeEnabled(outer, luid)
	if err != nil || outerBefore {
		t.Fatalf("test caller privilege was not disabled: %v", err)
	}
	sentinel := errors.New("creation failed")
	for _, want := range []error{nil, sentinel} {
		called := false
		err := withStorageSecurityPrivilege(func() error {
			called = true
			enabled, err := tokenPrivilegeEnabled(windows.GetCurrentThreadToken(), luid)
			if err != nil || !enabled {
				return errors.New("creation privilege is not enabled on worker")
			}
			return want
		})
		if !called || err != want {
			t.Fatalf("creation callback: called=%v error=%v want=%v", called, err, want)
		}
		if !bytes.Equal(creationTokenID(t, windows.GetCurrentThreadToken()), outerID) {
			t.Fatal("creation replaced the caller's existing impersonation")
		}
		if after, err := tokenPrivilegeEnabled(outer, luid); err != nil || after != outerBefore {
			t.Fatalf("creation changed caller privilege: %v -> %v (%v)", outerBefore, after, err)
		}
		after, err := tokenPrivilegeEnabled(source, luid)
		if err != nil || after != before {
			t.Fatalf("creation changed process privilege: %v -> %v (%v)", before, after, err)
		}
	}
	// Remove the privilege only from this disposable test token. A successful
	// BOOL from AdjustTokenPrivileges must not admit an unassigned privilege.
	removed := windows.Tokenprivileges{PrivilegeCount: 1,
		Privileges: [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: windows.SE_PRIVILEGE_REMOVED}}}
	if err := windows.AdjustTokenPrivileges(outer, false, &removed, 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	called := false
	err = withStorageSecurityPrivilege(func() error {
		called = true
		return nil
	})
	if err != windows.ERROR_PRIVILEGE_NOT_HELD || called {
		t.Fatalf("unassigned privilege admitted: called=%v error=%v", called, err)
	}
	if !bytes.Equal(creationTokenID(t, windows.GetCurrentThreadToken()), outerID) {
		t.Fatal("failed privilege adjustment replaced the caller token")
	}
	if after, err := tokenPrivilegeEnabled(source, luid); err != nil || after != before {
		t.Fatal("unassigned privilege test changed the process token")
	}
}

func TestCreationSecurityDoesNotSubstituteProcessForMediumCaller(t *testing.T) {
	withMediumIntegrity(t, func() {
		before := creationTokenID(t, windows.GetCurrentThreadToken())
		called := false
		err := withStorageSecurityPrivilege(func() error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatal("creation substituted process authority for a Medium caller")
		}
		if !bytes.Equal(creationTokenID(t, windows.GetCurrentThreadToken()), before) {
			t.Fatal("creation replaced the Medium caller's token")
		}
	})
}
