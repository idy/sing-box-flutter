// SPDX-License-Identifier: GPL-3.0-or-later
//go:build !windows

package main

import (
	"io"
	"os"
	"path/filepath"
)

// Host builds exist for protocol/configuration tests, not for running a VPN.
func isElevated() bool { return false }

func lockWorkingDirectory(path string) (io.Closer, error) {
	return os.OpenFile(filepath.Join(path, "core.lock"), os.O_CREATE|os.O_RDWR, 0o600)
}

func privateDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errDirectories
	}
	return os.Chmod(path, 0o700)
}
