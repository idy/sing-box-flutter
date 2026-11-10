// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	if canonical, err := prepareStateDirectory(path); canonical != "" || !errors.Is(err, errStateDirectory) {
		t.Fatalf("expected no path and sanitized state directory error, got %q, %v", canonical, err)
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
		if canonical, err := prepareStateDirectory(state); err != nil || canonical != state {
			t.Fatalf("expected original canonical path %q, got %q, %v", state, canonical, err)
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
	if _, err := prepareStateDirectory(state); err != nil {
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

func TestPrepareStateDirectoryAcceptsTrustedAncestorLinks(t *testing.T) {
	for _, kind := range []string{"absolute", "relative", "parent relative", "chained", "physical parent"} {
		t.Run(kind, func(t *testing.T) {
			root := stateTestRoot(t)
			home := filepath.Join(root, "var", "home")
			if err := os.MkdirAll(home, 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "home")
			target := home
			switch kind {
			case "relative":
				target = "var/home"
			case "parent relative":
				parent := filepath.Join(root, "aliases", "nested")
				if err := os.MkdirAll(parent, 0o755); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(parent, "home")
				target = "../../var/home"
			case "chained":
				if err := os.Symlink("var/home", filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
				target = "alias"
			case "physical parent":
				if err := os.Mkdir(filepath.Join(root, "var", "other"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("var/other", filepath.Join(root, "jump")); err != nil {
					t.Fatal(err)
				}
				target = "jump/../home"
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(link, ".local", "share", "app-state")
			want := filepath.Join(home, ".local", "share", "app-state")
			for range 2 {
				canonical, err := prepareStateDirectory(state)
				if err != nil || canonical != want {
					t.Fatalf("expected canonical state %q, got %q, %v", want, canonical, err)
				}
				for _, path := range []string{canonical, filepath.Join(canonical, "tmp")} {
					info, err := os.Lstat(path)
					if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
						t.Fatalf("expected private canonical directory: %v, %v", info, err)
					}
				}
			}
		})
	}
}

func TestPrepareStateDirectoryRejectsPrivateSymlinks(t *testing.T) {
	for _, part := range []string{"state", "tmp", "linked state", "linked tmp"} {
		t.Run(part, func(t *testing.T) {
			root := stateTestRoot(t)
			target, err := os.MkdirTemp(root, "private-target-")
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "home")
			if err := os.Mkdir(home, 0o755); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(home, "state")
			link := state
			if part == "tmp" || part == "linked tmp" {
				if err := os.Mkdir(state, 0o700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(state, "tmp")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if part == "linked state" || part == "linked tmp" {
				alias := filepath.Join(root, "alias")
				if err := os.Symlink(home, alias); err != nil {
					t.Fatal(err)
				}
				state = filepath.Join(alias, "state")
			}
			requireUnsafeState(t, state)
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatalf("symlink target must remain untouched: %v, %v", entries, err)
			}
		})
	}
}

func TestPrepareStateDirectoryRejectsUnsafeAncestorLinks(t *testing.T) {
	for _, kind := range []string{"writable link parent", "writable target ancestor", "group writable target ancestor", "unsafe before parent", "file target"} {
		t.Run(kind, func(t *testing.T) {
			root := stateTestRoot(t)
			unsafe := filepath.Join(root, "unsafe")
			if err := os.Mkdir(unsafe, 0o700); err != nil {
				t.Fatal(err)
			}
			target, err := os.MkdirTemp(root, "target-")
			if err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "home")
			switch kind {
			case "writable link parent":
				link = filepath.Join(unsafe, "home")
			case "writable target ancestor", "group writable target ancestor":
				target = filepath.Join(unsafe, "target")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
			case "unsafe before parent":
				// Keep '..' literal: cleaning this target would hide unsafe ancestry.
				target = unsafe + "/../" + filepath.Base(target)
			case "file target":
				target = filepath.Join(root, "file")
				if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			mode := os.FileMode(0o777)
			if kind == "group writable target ancestor" {
				mode = 0o770
			}
			if err := os.Chmod(unsafe, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			requireUnsafeState(t, filepath.Join(link, "state"))
			if kind != "file target" {
				entries, err := os.ReadDir(target)
				if err != nil || len(entries) != 0 {
					t.Fatalf("unsafe link target must remain untouched: %v, %v", entries, err)
				}
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

func TestPrepareStateDirectoryRejectsWrongOwnerAncestorLinks(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("changing symlink and directory ownership requires root")
	}
	for _, part := range []string{"link", "parent", "target"} {
		t.Run(part, func(t *testing.T) {
			root := stateTestRoot(t)
			target, err := os.MkdirTemp(root, "target-")
			if err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(root, "links")
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(parent, "home")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			switch part {
			case "link":
				err = os.Lchown(link, 65534, -1)
			case "parent":
				err = os.Chown(parent, 65534, -1)
			case "target":
				err = os.Chown(target, 65534, -1)
			}
			if err != nil {
				t.Fatal(err)
			}
			requireUnsafeState(t, filepath.Join(link, "state"))
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatalf("foreign link target must remain untouched: %v, %v", entries, err)
			}
		})
	}
}

func TestPrepareStateDirectoryBoundsAncestorLinks(t *testing.T) {
	for _, kind := range []string{"self cycle", "two link cycle", "expanding cycle", "40 links", "41 links"} {
		t.Run(kind, func(t *testing.T) {
			root := stateTestRoot(t)
			link := filepath.Join(root, "home")
			target := "home"
			switch kind {
			case "two link cycle":
				target = "other"
				if err := os.Symlink("home", filepath.Join(root, target)); err != nil {
					t.Fatal(err)
				}
			case "expanding cycle":
				target = "home/child"
			case "40 links", "41 links":
				home := filepath.Join(root, "actual-home")
				if err := os.Mkdir(home, 0o755); err != nil {
					t.Fatal(err)
				}
				count := 40
				if kind == "41 links" {
					count++
				}
				target = "actual-home"
				for index := 1; index < count; index++ {
					name := "link-" + strconv.Itoa(index)
					if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
						t.Fatal(err)
					}
					target = name
				}
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(link, "state")
			if kind == "40 links" {
				want := filepath.Join(root, "actual-home", "state")
				if canonical, err := prepareStateDirectory(state); err != nil || canonical != want {
					t.Fatalf("expected bounded chain to resolve to %q, got %q, %v", want, canonical, err)
				}
			} else {
				requireUnsafeState(t, state)
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
