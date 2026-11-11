// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func directoryTestCacheRoot(t *testing.T) string {
	t.Helper()
	// Resolve the temporary root so macOS's /var symlink does not obscure the
	// behavior under test: production still rejects all ancestor symlinks.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	switch runtime.GOOS {
	case "windows":
		if !isElevated() {
			t.Skip("secure Windows storage requires an elevated test process")
		}
		t.Setenv("LOCALAPPDATA", root)
	case "darwin":
		t.Setenv("HOME", root)
		if err := os.MkdirAll(filepath.Join(root, "Library", "Caches"), 0o777); err != nil {
			t.Fatal(err)
		}
	default:
		t.Setenv("XDG_CACHE_HOME", root)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func TestInitializeDirectoriesPreservesHostStorageMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows permissions are covered by the ACL regression test")
	}
	base := directoryTestCacheRoot(t)
	host := filepath.Join(base, "Host App")
	if err := os.Mkdir(host, 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(host, 0o751); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(host, "host-owned.txt")
	if err := os.WriteFile(marker, []byte("host data"), 0o644); err != nil {
		t.Fatal(err)
	}
	working := filepath.Join(host, "sing_box_flutter")
	for range 2 {
		if err := initializeDirectories(working); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(host)
		if err != nil || info.Mode().Perm() != 0o751 {
			t.Fatalf("host storage mode changed: %v, %v", info, err)
		}
		for _, directory := range []string{working, filepath.Join(working, "cache"), filepath.Join(working, "tmp")} {
			info, err := os.Stat(directory)
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("plugin directory is not private: %s, %v, %v", directory, info, err)
			}
		}
	}
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "host data" {
		t.Fatalf("host-owned data changed: %q, %v", contents, err)
	}
}

func TestInitializeDirectoriesCreatesMissingHostAndRejectsUnsafeAncestors(t *testing.T) {
	base := directoryTestCacheRoot(t)
	working := filepath.Join(base, "New Host", "sing_box_flutter")
	if err := initializeDirectories(working); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Dir(working)); err != nil || !info.IsDir() {
		t.Fatalf("missing host directory was not created: %v", err)
	}
	file := filepath.Join(base, "File Host")
	if err := os.WriteFile(file, []byte("host file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if initializeDirectories(filepath.Join(file, "sing_box_flutter")) == nil {
		t.Fatal("accepted a non-directory ancestor")
	}
	outside := t.TempDir()
	link := filepath.Join(base, "Linked Host")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if initializeDirectories(filepath.Join(link, "sing_box_flutter")) == nil {
		t.Fatal("accepted a symlink ancestor")
	}
	if _, err := os.Stat(filepath.Join(outside, "sing_box_flutter")); !os.IsNotExist(err) {
		t.Fatalf("created private directories outside application storage: %v", err)
	}
}
