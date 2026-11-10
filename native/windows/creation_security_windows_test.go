// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"bytes"
	"errors"
	"runtime"
	"testing"

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

func TestCreationSecurityPreservesCallerAndProcessOnSuccessAndFailure(t *testing.T) {
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
