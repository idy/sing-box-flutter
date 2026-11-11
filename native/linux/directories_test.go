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

func temporaryStateForTest(t *testing.T) (string, *temporaryStateDirectory) {
	t.Helper()
	state, err := os.MkdirTemp(stateTestRoot(t), "ephemeral-")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := prepareStateDirectory(state)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := pinTemporaryStateDirectory(canonical)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pin.Close(); err != nil {
			t.Error(err)
		}
	})
	return state, pin
}

func TestTemporaryStateDirectoryRemovesPopulatedTree(t *testing.T) {
	t.Setenv("UID", "123456789")
	t.Setenv("USER", "unrelated-user")
	state, pin := temporaryStateForTest(t)
	for _, path := range []string{"auth/session", "cache/dns/cache.db", "tmp/download/partial", "cache.db"} {
		path = filepath.Join(state, path)
		// Nested state need not be 0700; the pinned state root provides privacy.
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("private state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pin.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected temporary state root to be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(state)); err != nil {
		t.Fatalf("temporary state parent must remain: %v", err)
	}
	if err := pin.Close(); err != nil {
		t.Fatalf("closing removed state must be harmless: %v", err)
	}
}

func TestTemporaryStateDirectoryUnlinksSymlinksWithoutFollowing(t *testing.T) {
	state, pin := temporaryStateForTest(t)
	outside := stateTestRoot(t)
	marker := filepath.Join(outside, "keep")
	if err := os.WriteFile(marker, []byte("outside data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"directory": outside,
		"file":      marker,
		"dangling":  filepath.Join(outside, "missing"),
		"parent":    "..",
		"self":      ".",
	} {
		if err := os.Symlink(target, filepath.Join(state, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := pin.Remove(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "outside data" {
		t.Fatalf("symlink target must remain untouched: %q, %v", content, err)
	}
}

func TestTemporaryStateDirectoryClosePreservesState(t *testing.T) {
	state, pin := temporaryStateForTest(t)
	for range 2 {
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := pin.Remove(); !errors.Is(err, errStateDirectory) {
		t.Fatalf("closed state must refuse removal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "tmp")); err != nil {
		t.Fatalf("Close must preserve state: %v", err)
	}
	var empty temporaryStateDirectory
	if err := empty.Remove(); !errors.Is(err, errStateDirectory) {
		t.Fatalf("zero-value state must refuse removal: %v", err)
	}
}

func TestTemporaryStateDirectoryRefusesReplacedRoot(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "missing"} {
		t.Run(kind, func(t *testing.T) {
			state, pin := temporaryStateForTest(t)
			marker := filepath.Join(state, "keep")
			if err := os.WriteFile(marker, []byte("original state"), 0o600); err != nil {
				t.Fatal(err)
			}
			moved := state + "-moved"
			if err := os.Rename(state, moved); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "directory":
				if err := os.Mkdir(state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(marker, []byte("replacement state"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(moved, state); err != nil {
					t.Fatal(err)
				}
			}
			if err := pin.Remove(); !errors.Is(err, errStateDirectory) {
				t.Fatalf("replaced root must refuse cleanup: %v", err)
			}
			content, err := os.ReadFile(filepath.Join(moved, "keep"))
			if err != nil || string(content) != "original state" {
				t.Fatalf("original state must remain untouched: %q, %v", content, err)
			}
			if kind == "directory" {
				content, err = os.ReadFile(marker)
				if err != nil || string(content) != "replacement state" {
					t.Fatalf("replacement state must remain untouched: %q, %v", content, err)
				}
			} else if kind == "symlink" {
				if info, err := os.Lstat(state); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("replacement symlink must remain untouched: %v, %v", info, err)
				}
			}
		})
	}
}

func TestTemporaryStateDirectoryRefusesReplacedAncestor(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			state, pin := temporaryStateForTest(t)
			parent := filepath.Dir(state)
			moved := parent + "-moved"
			if err := os.Rename(parent, moved); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(moved); err != nil {
					t.Error(err)
				}
			})
			if kind == "symlink" {
				if err := os.Symlink(moved, parent); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(filepath.Join(state, "tmp"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := pin.Remove(); !errors.Is(err, errStateDirectory) {
				t.Fatalf("replaced ancestor must refuse cleanup: %v", err)
			}
			for _, path := range []string{state, filepath.Join(moved, filepath.Base(state))} {
				if _, err := os.Stat(filepath.Join(path, "tmp")); err != nil {
					t.Fatalf("original and replacement state must remain: %s: %v", path, err)
				}
			}
		})
	}
}

func TestPinTemporaryStateDirectoryRejectsUnsafePaths(t *testing.T) {
	state, pin := temporaryStateForTest(t)
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(state), "file")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := state + "-link"
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	missing := state + "-missing"
	for _, path := range []string{"", ".", "..", "/", "relative/state", state + "/", state + "/../" + filepath.Base(state), file, link, filepath.Join(link, "tmp"), missing} {
		if got, err := pinTemporaryStateDirectory(path); got != nil || !errors.Is(err, errStateDirectory) {
			if got != nil {
				_ = got.Close()
			}
			t.Fatalf("unsafe path %q must refuse pinning: %v, %v", path, got, err)
		}
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinning must never create missing state: %v", err)
	}
	if err := os.Chmod(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := pinTemporaryStateDirectory(state); got != nil || !errors.Is(err, errStateDirectory) {
		if got != nil {
			_ = got.Close()
		}
		t.Fatalf("non-private state must refuse pinning: %v, %v", got, err)
	}
}

func TestTemporaryStateDirectoryRefusesChangedPermissions(t *testing.T) {
	state, pin := temporaryStateForTest(t)
	if err := os.Chmod(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pin.Remove(); !errors.Is(err, errStateDirectory) {
		t.Fatalf("changed permissions must refuse cleanup: %v", err)
	}
	if info, err := os.Stat(filepath.Join(state, "tmp")); err != nil || !info.IsDir() {
		t.Fatalf("unsafe state must remain untouched: %v, %v", info, err)
	}
}

func TestTemporaryStateDirectoryRefusesForeignOwnership(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("changing directory ownership requires root")
	}
	for _, part := range []string{"root", "ancestor", "child", "file"} {
		t.Run(part, func(t *testing.T) {
			state, pin := temporaryStateForTest(t)
			foreign := state
			switch part {
			case "ancestor":
				foreign = filepath.Dir(state)
			case "child":
				foreign = filepath.Join(state, "tmp")
			case "file":
				foreign = filepath.Join(state, "foreign")
				if err := os.WriteFile(foreign, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chown(foreign, 65534, -1); err != nil {
				t.Fatal(err)
			}
			if part == "root" {
				if got, err := pinTemporaryStateDirectory(state); got != nil || !errors.Is(err, errStateDirectory) {
					if got != nil {
						_ = got.Close()
					}
					t.Fatalf("foreign state must refuse pinning: %v, %v", got, err)
				}
			}
			if err := pin.Remove(); !errors.Is(err, errStateDirectory) {
				t.Fatalf("foreign ownership must refuse cleanup: %v", err)
			}
			if _, err := os.Lstat(foreign); err != nil {
				t.Fatalf("foreign entry must remain untouched: %v", err)
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
