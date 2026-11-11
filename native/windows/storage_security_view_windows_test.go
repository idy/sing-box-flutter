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

func TestStorageRootValidatesFullSACLProtection(t *testing.T) {
	for _, protected := range []bool{true, false} {
		name := "protected"
		if !protected {
			name = "unprotected"
		}
		t.Run(name, func(t *testing.T) {
			base := directoryTestCacheRoot(t)
			hostPath := filepath.Join(base, "Host App")
			if err := os.Mkdir(hostPath, 0o700); err != nil {
				t.Fatal(err)
			}
			descriptor, err := privateDirectorySecurity()
			if err != nil {
				t.Fatal(err)
			}
			if !protected {
				sddl := descriptor.String()
				if !strings.Contains(sddl, "S:P") {
					t.Fatal("fixture lacks protected SACL")
				}
				descriptor, err = windows.SecurityDescriptorFromString(strings.Replace(sddl, "S:P", "S:", 1))
				if err != nil {
					t.Fatal(err)
				}
			}
			directories, err := openDirectoryHierarchy(hostPath)
			if err != nil {
				t.Fatal(err)
			}
			defer directories.close()
			handle, err := directories.openPrivateRoot(directories.last(), "sing_box_flutter", descriptor)
			if err != nil {
				t.Fatal(err)
			}
			labelView, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
				windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			labelControl, _, err := labelView.Control()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("label-only descriptor control: 0x%04x", labelControl)
			if err := verifyPrivateRoot(handle); (err == nil) != protected {
				t.Fatalf("root SACL protection accepted=%v expected=%v: %v", err == nil, protected, err)
			}
		})
	}
}

func TestWindowsStorageInitializationReportsStageAndReopens(t *testing.T) {
	base := directoryTestCacheRoot(t)
	working := filepath.Join(base, "Host App", "sing_box_flutter")
	for range 2 {
		lease, err := initializeWindowsStorage(working, base, windows.GetCurrentProcessToken())
		if err != nil {
			// Unlike the public sanitized initializer, retain the exact internal
			// stage and Win32 error for this owned test fixture.
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
