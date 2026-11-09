// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2022 nekohasekai <contact-sagernet@sekai.icu>
// Adapted from SagerNet/sing-box v1.14.4; see NOTICE for the pinned revision.

package libbox

import "golang.org/x/sys/unix"

func getTunnelName(fd int32) (string, error) {
	return unix.GetsockoptString(
		int(fd),
		2, /* #define SYSPROTO_CONTROL 2 */
		2, /* #define UTUN_OPT_IFNAME 2 */
	)
}
