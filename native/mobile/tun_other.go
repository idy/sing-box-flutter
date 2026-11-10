// SPDX-License-Identifier: GPL-3.0-or-later
//go:build !darwin

package libbox

func GetTunnelFileDescriptor() int32 { return -1 }
