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

// temporaryStateDirectory is an explicit opt-in to deleting ephemeral state.
// Keep it open while libbox uses the directory, and call Remove only after all
// service workers have stopped. Close alone never removes state.
// Like the helper's lifecycle, its methods are used serially.
type temporaryStateDirectory struct {
	directories []temporaryStateDirectoryPin
	uid         uint32
}

type temporaryStateDirectoryPin struct {
	fd   int
	name string
	stat unix.Stat_t
}

// pinTemporaryStateDirectory takes the canonical path from prepareStateDirectory.
// It never creates directories or follows links, including in ancestor components.
// Pinning the entire chain lets cleanup refuse a renamed/recreated ancestor as
// well as a replaced state directory, without reopening the original pathname.
func pinTemporaryStateDirectory(canonical string) (*temporaryStateDirectory, error) {
	if !filepath.IsAbs(canonical) || filepath.Clean(canonical) != canonical || canonical == "/" {
		return nil, errStateDirectory
	}
	state := &temporaryStateDirectory{uid: uint32(os.Getuid())}
	names := append([]string{"/"}, strings.Split(strings.TrimPrefix(canonical, "/"), "/")...)
	for index, name := range names {
		parent := unix.AT_FDCWD
		if index != 0 {
			parent = state.directories[index-1].fd
		}
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			_ = state.Close()
			return nil, errStateDirectory
		}
		pin := temporaryStateDirectoryPin{fd: fd, name: name}
		private := index == len(names)-1
		if unix.Fstat(fd, &pin.stat) != nil || !stateDirectoryAllowed(fd, state.uid, private) ||
			(private && pin.stat.Mode&0o777 != 0o700) {
			_ = unix.Close(fd)
			_ = state.Close()
			return nil, errStateDirectory
		}
		state.directories = append(state.directories, pin)
	}
	if !state.unchanged() {
		_ = state.Close()
		return nil, errStateDirectory
	}
	return state, nil
}

func sameTemporaryStateDirectory(stat, pinned unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFDIR && stat.Dev == pinned.Dev &&
		stat.Ino == pinned.Ino && stat.Uid == pinned.Uid
}

func (state *temporaryStateDirectory) unchanged() bool {
	if state == nil || len(state.directories) < 2 || uint32(os.Getuid()) != state.uid {
		return false
	}
	for index, pin := range state.directories {
		var stat unix.Stat_t
		private := index == len(state.directories)-1
		if unix.Fstat(pin.fd, &stat) != nil || !sameTemporaryStateDirectory(stat, pin.stat) ||
			!stateDirectoryAllowed(pin.fd, state.uid, private) || (private && stat.Mode&0o777 != 0o700) {
			return false
		}
		if index != 0 && (unix.Fstatat(state.directories[index-1].fd, pin.name, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil ||
			!sameTemporaryStateDirectory(stat, pin.stat)) {
			return false
		}
	}
	return true
}

// Remove checks the original identities before deleting anything, removes
// contents through pinned descriptors, then checks again before unlinking the
// empty leaf. It always closes the pins, including when cleanup is refused.
func (state *temporaryStateDirectory) Remove() (err error) {
	defer func() { err = errors.Join(err, state.Close()) }()
	if !state.unchanged() {
		return errStateDirectory
	}
	leaf := state.directories[len(state.directories)-1]
	if err := removeTemporaryStateContents(leaf.fd, state.uid, leaf.stat.Dev); err != nil {
		return err
	}
	if !state.unchanged() {
		return errStateDirectory
	}
	parent := state.directories[len(state.directories)-2]
	if unix.Unlinkat(parent.fd, leaf.name, unix.AT_REMOVEDIR) != nil {
		return errStateDirectory
	}
	return nil
}

// Close releases the pinned descriptors without removing any directory or file.
func (state *temporaryStateDirectory) Close() error {
	if state == nil {
		return nil
	}
	var err error
	for _, pin := range state.directories {
		if unix.Close(pin.fd) != nil {
			err = errStateDirectory
		}
	}
	state.directories = nil
	return err
}

func removeTemporaryStateContents(directory int, uid uint32, device uint64) error {
	// Use a fresh descriptor for enumeration, so its position is independent of
	// the pin. Read all names before unlinking to avoid skipping reshuffled entries.
	fd, err := unix.Openat(directory, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errStateDirectory
	}
	file := os.NewFile(uintptr(fd), "temporary state directory")
	names, readErr := file.Readdirnames(-1)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errStateDirectory
	}
	for _, name := range names {
		var stat unix.Stat_t
		if unix.Fstatat(directory, name, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil || stat.Uid != uid || stat.Dev != device {
			return errStateDirectory
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			// This unlinks symlinks themselves and never opens their targets.
			if unix.Unlinkat(directory, name, 0) != nil {
				return errStateDirectory
			}
			continue
		}
		child, err := unix.Openat(directory, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return errStateDirectory
		}
		var opened unix.Stat_t
		if unix.Fstat(child, &opened) != nil || !sameTemporaryStateDirectory(opened, stat) {
			_ = unix.Close(child)
			return errStateDirectory
		}
		err = removeTemporaryStateContents(child, uid, device)
		_ = unix.Close(child)
		if err != nil {
			return err
		}
		var current unix.Stat_t
		if unix.Fstatat(directory, name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil ||
			!sameTemporaryStateDirectory(current, stat) || unix.Unlinkat(directory, name, unix.AT_REMOVEDIR) != nil {
			return errStateDirectory
		}
	}
	return nil
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
