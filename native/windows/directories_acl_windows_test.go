// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestInitializeDirectoriesPreservesHostStorageACL(t *testing.T) {
	base := directoryTestCacheRoot(t)
	host := filepath.Join(base, "Host App")
	if err := os.Mkdir(host, 0o777); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// The host grants builtin Users read access. The plugin's stricter DACL
	// must not remove that host-owned policy while securing its own subtree.
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;GR;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(host, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(host, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	working := filepath.Join(host, "sing_box_flutter")
	for range 2 {
		if err := initializeDirectories(working); err != nil {
			t.Fatal(err)
		}
		after, err := windows.GetNamedSecurityInfo(host, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if after.String() != before.String() {
			t.Fatalf("host storage DACL changed: got %s, want %s", after.String(), before.String())
		}
		for _, directory := range []string{working, filepath.Join(working, "cache"), filepath.Join(working, "tmp")} {
			private, err := windows.GetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			if text := private.String(); !strings.Contains(text, "D:P") || strings.Contains(text, ";;;BU)") {
				t.Fatalf("plugin directory inherited host access: %s", text)
			}
		}
	}
}
