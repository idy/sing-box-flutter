// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var errStateDirectory = errors.New("invalid private state directory")

// prepareStateDirectory creates private state and temporary directories before
// libbox.Setup can open them. Existing unsafe paths are rejected, never chmodded.
func prepareStateDirectory(dataDir string) error {
	if !filepath.IsAbs(dataDir) || filepath.Clean(dataDir) != dataDir || dataDir == "/" {
		return errStateDirectory
	}
	// File capabilities do not change the real UID. Do not trust USER, UID or
	// similar environment variables supplied to this capability-bearing helper.
	uid := uint32(os.Getuid())
	parent, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errStateDirectory
	}
	defer func() { _ = unix.Close(parent) }()
	if !stateDirectoryAllowed(parent, uid, false) {
		return errStateDirectory
	}
	components := strings.Split(strings.TrimPrefix(dataDir, "/"), "/")
	for index, component := range components {
		child, err := openStateChild(parent, component, uid, index == len(components)-1)
		if err != nil {
			return err
		}
		_ = unix.Close(parent)
		parent = child
	}
	temporary, err := openStateChild(parent, "tmp", uid, true)
	if err != nil {
		return err
	}
	_ = unix.Close(temporary)
	return nil
}

func openStateChild(parent int, name string, uid uint32, private bool) (int, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	child, err := unix.Openat(parent, name, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(parent, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, errStateDirectory
		}
		child, err = unix.Openat(parent, name, flags, 0)
	}
	if err != nil {
		return -1, errStateDirectory
	}
	if !stateDirectoryAllowed(child, uid, private) {
		_ = unix.Close(child)
		return -1, errStateDirectory
	}
	return child, nil
}

func stateDirectoryAllowed(directory int, uid uint32, private bool) bool {
	var stat unix.Stat_t
	if unix.Fstat(directory, &stat) != nil {
		return false
	}
	if private {
		return stat.Uid == uid && stat.Mode&0o077 == 0
	}
	// Ancestors may be ordinary user directories (0755) or root directories.
	// Only root-owned sticky directories, such as /tmp, may be shared writable:
	// sticky ownership protects our checked children from other users' renames.
	if stat.Uid != uid && stat.Uid != 0 {
		return false
	}
	return stat.Mode&0o022 == 0 || (stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0)
}
