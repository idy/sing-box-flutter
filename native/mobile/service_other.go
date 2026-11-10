// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2022 nekohasekai <contact-sagernet@sekai.icu>
// Adapted from SagerNet/sing-box v1.14.4; see NOTICE for the pinned revision.

//go:build !windows

package libbox

import "golang.org/x/sys/unix"

func dup(fd int) (nfd int, err error) {
	// Descriptor zero means "create a new TUN" to sing-tun; never return it.
	return unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 1)
}
