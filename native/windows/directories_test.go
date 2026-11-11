// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkingPathIsBoundToPerApplicationLocalData(t *testing.T) {
	base := t.TempDir()
	valid := filepath.Join(base, "Host App", "sing_box_flutter")
	if err := validateWorkingPath(valid, base); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "relative/sing_box_flutter", base,
		filepath.Join(base, "sing_box_flutter"), filepath.Join(base, "host", "other"),
		filepath.Join(base, "host", "nested", "sing_box_flutter"),
		filepath.Join(filepath.Dir(base), "other", "sing_box_flutter"),
		filepath.Join(base, "host.", "sing_box_flutter"), filepath.Join(base, "host:stream", "sing_box_flutter"),
		valid + string(filepath.Separator) + ".."} {
		if validateWorkingPath(path, base) == nil {
			t.Errorf("unsafe working path accepted: %q", path)
		}
	}
}

func TestStorageComponentsAreValidatedLexically(t *testing.T) {
	for _, name := range []string{"Host App", " Leading space", " leading.dot", "normal.txt", ".state", "COM0", "LPT10"} {
		if !safeDirectoryComponent(name) {
			t.Errorf("rejected valid literal component %q", name)
		}
		base := t.TempDir()
		if err := validateWorkingPath(filepath.Join(base, name, "sing_box_flutter"), base); err != nil {
			t.Errorf("rejected valid host component %q: %v", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "trailing ", "trailing.", "a/b", `a\b`, "a:stream", "a\x00b", "a\x1fb", "a\x7fb",
		"CON", "con.txt", "NUL.txt", "nul .txt", "PRN", "AUX", "CONIN$", "CONOUT$.txt", "CLOCK$", "COM1.log", "LPT9.txt", "COM¹.txt", "LPT²"} {
		if safeDirectoryComponent(name) {
			t.Errorf("accepted ambiguous/unsafe component %q", name)
		}
	}
}

func TestPrivateDirectoryRejectsLink(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(base, "link")
	if err := os.Symlink(outside, path); err != nil {
		t.Skip("symlinks unavailable")
	}
	if privateDirectory(path) == nil {
		t.Fatal("private directory followed a link")
	}
}
