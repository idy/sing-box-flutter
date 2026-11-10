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

func initializePlatformStorage(path, base string) (io.Closer, error) {
	if err := initializePlatformDirectories(path, base); err != nil {
		return nil, err
	}
	return lockWorkingDirectory(path)
}

func initializePlatformDirectories(path, _ string) error {
	// This path-based implementation is only a host-test shim. Windows uses
	// pinned handles for both creation and ACL writes in its elevated helper.
	for ancestor := path; ; ancestor = filepath.Dir(ancestor) {
		info, err := os.Lstat(ancestor)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errDirectories
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if ancestor == filepath.Dir(ancestor) {
			break
		}
	}
	if err := os.Mkdir(filepath.Dir(path), 0o777); err != nil && !os.IsExist(err) {
		return err
	}
	for _, directory := range []string{path, filepath.Join(path, "cache"), filepath.Join(path, "tmp")} {
		if err := privateDirectory(directory); err != nil {
			return err
		}
	}
	return nil
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
