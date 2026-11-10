// SPDX-License-Identifier: GPL-3.0-or-later
//go:build !android

package libbox

import "errors"

func FindConnectionOwner(ipProtocol int32, sourceAddress string, sourcePort int32, destinationAddress string, destinationPort int32) (*ConnectionOwner, error) {
	return nil, errors.New("connection ownership lookup is unsupported on this platform")
}
