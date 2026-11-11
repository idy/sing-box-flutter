// SPDX-License-Identifier: GPL-3.0-or-later
//go:build !darwin

package libbox

import (
	"errors"

	tun "github.com/sagernet/sing-tun"
)

func readStandaloneRoutes() ([]standaloneRoute, error) {
	return nil, errors.New("standalone routes require macOS")
}

func openStandaloneTun(tun.Options) (tun.Tun, error) {
	return nil, errors.New("standalone TUN requires macOS")
}
