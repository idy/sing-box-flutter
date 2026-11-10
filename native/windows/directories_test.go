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
