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
// libbox.Setup can open them. It returns the validated canonical path so Setup
// does not follow the original ancestor links again. Unsafe paths are never chmodded.
func prepareStateDirectory(dataDir string) (string, error) {
	if !filepath.IsAbs(dataDir) || filepath.Clean(dataDir) != dataDir || dataDir == "/" {
		return "", errStateDirectory
	}
	// File capabilities do not change the real UID. Do not trust USER, UID or
	// similar environment variables supplied to this capability-bearing helper.
	uid := uint32(os.Getuid())
	root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", errStateDirectory
	}
	parents := []int{root}
	defer func() {
		for _, directory := range parents {
			_ = unix.Close(directory)
		}
	}()
	if !stateDirectoryAllowed(root, uid, false) {
		return "", errStateDirectory
	}
	var canonical []string
	components := strings.Split(strings.TrimPrefix(dataDir, "/"), "/")
	links := 0
	for len(components) != 0 {
		component := components[0]
		components = components[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(parents) > 1 {
				_ = unix.Close(parents[len(parents)-1])
				parents = parents[:len(parents)-1]
				canonical = canonical[:len(canonical)-1]
			}
			continue
		}
		parent := parents[len(parents)-1]
		private := len(components) == 0
		child, err := openStateChild(parent, component, uid, private)
		if err == nil {
			parents = append(parents, child)
			canonical = append(canonical, component)
			continue
		}
		// Only ancestor links may be followed, and both their parents and their
		// resolved directory chains must be trusted. Match Linux's link limit to
		// bound cycles, including chains that revisit a different spelling.
		if private || links == 40 {
			return "", errStateDirectory
		}
		target, err := stateSymlinkTarget(parent, component, uid)
		if err != nil {
			return "", errStateDirectory
		}
		links++
		if filepath.IsAbs(target) {
			for _, directory := range parents[1:] {
				_ = unix.Close(directory)
			}
			parents = parents[:1]
			canonical = canonical[:0]
		}
		// Do not clean the target: a component before '..' can itself be a link
		// or unsafe directory and must be resolved and checked before stepping up.
		components = append(strings.Split(target, "/"), components...)
	}
	temporary, err := openStateChild(parents[len(parents)-1], "tmp", uid, true)
	if err != nil {
		return "", err
	}
	_ = unix.Close(temporary)
	return "/" + strings.Join(canonical, "/"), nil
}

func stateSymlinkTarget(parent int, name string, uid uint32) (string, error) {
	// Pin the link itself: ownership inspection and readlink must observe the
	// same inode even if a trusted owner replaces the directory entry meanwhile.
	link, err := unix.Openat(parent, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", errStateDirectory
	}
	defer func() { _ = unix.Close(link) }()
	var stat unix.Stat_t
	if unix.Fstat(link, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFLNK || (stat.Uid != uid && stat.Uid != 0) {
		return "", errStateDirectory
	}
	buffer := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(link, "", buffer)
	if err != nil || n == 0 || n == len(buffer) {
		return "", errStateDirectory
	}
	return string(buffer[:n]), nil
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
