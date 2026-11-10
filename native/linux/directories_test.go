// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func stateTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func requireUnsafeState(t *testing.T, path string) {
	t.Helper()
	if err := prepareStateDirectory(path); !errors.Is(err, errStateDirectory) {
		t.Fatalf("expected sanitized state directory error, got %v", err)
	}
}

func TestPrepareStateDirectoryFreshAndReusable(t *testing.T) {
	// Environment names cannot choose the owner of a capability-bearing helper.
	t.Setenv("UID", "123456789")
	t.Setenv("USER", "unrelated-user")
	root := stateTestRoot(t)
	home := filepath.Join(root, "home", ".local", "share")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "installation", "sing_box_flutter")
	for range 2 {
		if err := prepareStateDirectory(state); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{state, filepath.Join(state, "tmp")} {
			info, err := os.Stat(path)
			if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Fatalf("expected private directory: %v, %v", info, err)
			}
		}
	}
}

func TestPrepareStateDirectoryAcceptsFreshTemporaryDirectory(t *testing.T) {
	state, err := os.MkdirTemp(stateTestRoot(t), "sing-box-flutter-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(state); err != nil {
			t.Error(err)
		}
	})
	if err := prepareStateDirectory(state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "tmp")); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareStateDirectoryRejectsNonPrivatePaths(t *testing.T) {
	for _, test := range []struct {
		name string
		part string
		mode os.FileMode
	}{
		{"readable state", "state", 0o755},
		{"writable state", "state", 0o777},
		{"group accessible state", "state", 0o770},
		{"readable temporary directory", "state/tmp", 0o755},
		{"writable ancestor", "", 0o777},
		{"group writable ancestor", "", 0o770},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := stateTestRoot(t)
			state := filepath.Join(root, "state")
			unsafe := filepath.Join(root, test.part)
			if err := os.MkdirAll(unsafe, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(unsafe, test.mode); err != nil {
				t.Fatal(err)
			}
			requireUnsafeState(t, state)
			info, err := os.Stat(unsafe)
			if err != nil || info.Mode().Perm() != test.mode {
				t.Fatalf("unsafe permissions must not be repaired: %v, %v", info, err)
			}
		})
	}
}

func TestPrepareStateDirectoryRejectsSymlinks(t *testing.T) {
	for _, part := range []string{"state", "ancestor", "tmp"} {
		t.Run(part, func(t *testing.T) {
			root := stateTestRoot(t)
			target := filepath.Join(root, "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(root, "state")
			link := state
			if part == "ancestor" {
				link = filepath.Join(root, "alias")
				state = filepath.Join(link, "state")
			} else if part == "tmp" {
				if err := os.Mkdir(state, 0o700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(state, "tmp")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			requireUnsafeState(t, state)
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatalf("symlink target must remain untouched: %v, %v", entries, err)
			}
		})
	}
}

func TestPrepareStateDirectoryRejectsWrongOwner(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("changing directory ownership requires root")
	}
	for _, part := range []string{"state", "ancestor", "state/tmp"} {
		t.Run(part, func(t *testing.T) {
			root := stateTestRoot(t)
			state := filepath.Join(root, "state")
			foreign := filepath.Join(root, part)
			if part == "ancestor" {
				state = filepath.Join(foreign, "state")
			}
			if err := os.MkdirAll(foreign, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(foreign, 65534, -1); err != nil {
				t.Fatal(err)
			}
			requireUnsafeState(t, state)
			info, err := os.Stat(foreign)
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("foreign permissions must remain unchanged: %v, %v", info, err)
			}
		})
	}
}

func TestPrepareStateDirectoryRejectsFilesAndRelativePaths(t *testing.T) {
	root := stateTestRoot(t)
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", ".", "relative/state", "/", file, filepath.Join(file, "state")} {
		requireUnsafeState(t, path)
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != "untouched" {
		t.Fatalf("existing file must remain untouched: %q, %v", content, err)
	}
}
