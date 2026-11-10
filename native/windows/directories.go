// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var errDirectories = errors.New("invalid private application directories")

func validateWorkingPath(path, localAppData string) error {
	if path == "" || localAppData == "" || !filepath.IsAbs(path) ||
		!filepath.IsAbs(localAppData) || filepath.Clean(path) != path {
		return errDirectories
	}
	relative, err := filepath.Rel(localAppData, path)
	if err != nil {
		return errDirectories
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || parts[1] != "sing_box_flutter" {
		return errDirectories
	}
	stem := parts[0]
	if stem == "" || stem == "." || stem == ".." || strings.TrimSpace(stem) != stem ||
		strings.HasSuffix(stem, ".") || strings.ContainsAny(stem, `<>:"/\|?*`+"\x00") {
		return errDirectories
	}
	return nil
}

func initializeDirectories(path string) error {
	base, err := os.UserCacheDir() // LOCALAPPDATA on Windows, never the exe directory.
	if err != nil || validateWorkingPath(path, base) != nil {
		return errDirectories
	}
	// Reject symlinks/junctions in existing ancestors before creating or securing
	// any directories. The host's executable stem is one path component only.
	for ancestor := path; ; ancestor = filepath.Dir(ancestor) {
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errDirectories
			}
		} else if !os.IsNotExist(statErr) {
			return errDirectories
		}
		if ancestor == filepath.Dir(ancestor) {
			break
		}
	}
	// The host owns its storage parent and may share it with other components.
	// Create it with ordinary permissions, and secure only the plugin subtree.
	if err = os.Mkdir(filepath.Dir(path), 0o777); err != nil && !os.IsExist(err) {
		return errDirectories
	}
	for _, directory := range []string{path, filepath.Join(path, "cache"), filepath.Join(path, "tmp")} {
		if err = privateDirectory(directory); err != nil {
			return errDirectories
		}
	}
	return nil
}
