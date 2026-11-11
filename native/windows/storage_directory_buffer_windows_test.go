// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestStorageDirectoryEntriesAcrossBatches(t *testing.T) {
	if unsafe.Offsetof(storageDirectoryInfo{}.FileName) != 104 || unsafe.Alignof(storageDirectoryInfo{}) != 8 {
		t.Fatal("directory record does not match the Windows ABI")
	}
	path := t.TempDir()
	const count = 768 // Record headers alone exceed the 64 KiB query buffer.
	want := make(map[string]bool, count)
	for i := range count {
		name := fmt.Sprintf("native-state-%04d.bin", i)
		if err := os.WriteFile(filepath.Join(path, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		want[name] = true
	}
	directories, err := openDirectoryHierarchy(path)
	if err != nil {
		t.Fatal(err)
	}
	defer directories.close()
	for range 2 {
		entries, err := storageDirectoryEntries(directories.last())
		if err != nil {
			t.Fatalf("aligned handle-relative enumeration: %v", err)
		}
		if len(entries) != count {
			t.Fatalf("directory entries: got %d, want %d", len(entries), count)
		}
		seen := make(map[string]bool, count)
		ids := make(map[uint64]bool, count)
		for _, entry := range entries {
			if !want[entry.name] || seen[entry.name] || entry.id == 0 || ids[entry.id] {
				t.Fatalf("invalid or duplicate entry: %q, file ID %d", entry.name, entry.id)
			}
			seen[entry.name], ids[entry.id] = true, true
		}
	}
}
