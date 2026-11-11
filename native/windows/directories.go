// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
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
	if !safeDirectoryComponent(parts[0]) {
		return errDirectories
	}
	return nil
}

// This is lexical only: filepath.IsLocal on Windows can consult the filesystem
// when deciding whether an extension is a DOS device alias (for example NUL.txt).
// Leading spaces are literal valid characters; trailing spaces/dots are aliases.
func safeDirectoryComponent(name string) bool {
	if name == "" || strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") ||
		strings.ContainsAny(name, `<>:"/\|?*`) || strings.ContainsFunc(name, unicode.IsControl) {
		return false
	}
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$", "CLOCK$":
		return false
	}
	for _, prefix := range []string{"COM", "LPT"} {
		if strings.HasPrefix(stem, prefix) {
			suffix := strings.TrimPrefix(stem, prefix)
			if len(suffix) == 1 && suffix[0] >= '1' && suffix[0] <= '9' || suffix == "¹" || suffix == "²" || suffix == "³" {
				return false
			}
		}
	}
	return true
}

// Production owns this single lease until all engine/check workers have closed.
// Directory-only callers (including fixtures) can prepare storage and release it.
func initializeDirectories(path string) error {
	storage, err := initializeStorage(path)
	if err != nil {
		return err
	}
	return storage.Close()
}

func initializeStorage(path string) (io.Closer, error) {
	base, err := os.UserCacheDir() // LOCALAPPDATA on Windows, never the exe directory.
	if err != nil || validateWorkingPath(path, base) != nil {
		return nil, errDirectories
	}
	storage, err := initializePlatformStorage(path, base)
	if err != nil {
		return nil, errDirectories
	}
	return storage, nil
}

type storageContextKey struct{}

func withStorageContext(ctx context.Context, storage io.Closer) context.Context {
	return context.WithValue(ctx, storageContextKey{}, storage)
}
