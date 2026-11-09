// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2022 nekohasekai <contact-sagernet@sekai.icu>
// Adapted from SagerNet/sing-box v1.14.4; see NOTICE for the pinned revision.

package libbox

import (
	"net/netip"
	"syscall"

	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"
)

func FindConnectionOwner(ipProtocol int32, sourceAddress string, sourcePort int32, destinationAddress string, destinationPort int32) (*ConnectionOwner, error) {
	if sourcePort < 0 || sourcePort > 65535 || destinationPort < 0 || destinationPort > 65535 {
		return nil, E.New("invalid connection port")
	}
	var network string
	switch ipProtocol {
	case syscall.IPPROTO_TCP:
		network = N.NetworkTCP
	case syscall.IPPROTO_UDP:
		network = N.NetworkUDP
	default:
		return nil, E.New("unsupported connection protocol")
	}
	sourceIP, err := netip.ParseAddr(sourceAddress)
	if err != nil {
		return nil, E.New("invalid source address")
	}
	destinationIP, _ := netip.ParseAddr(destinationAddress)
	uid := resolveSocketByProcSearch(network, netip.AddrPortFrom(sourceIP.Unmap(), uint16(sourcePort)), netip.AddrPortFrom(destinationIP.Unmap(), uint16(destinationPort)))
	if uid == -1 {
		return nil, E.New("procfs: not found")
	}
	return &ConnectionOwner{UserId: uid}, nil
}
