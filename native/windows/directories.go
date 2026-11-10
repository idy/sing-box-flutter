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
	if err = initializePlatformDirectories(path, base); err != nil {
		return errDirectories
	}
	return nil
}
