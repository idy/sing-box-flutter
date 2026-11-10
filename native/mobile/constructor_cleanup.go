// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
)

// These pinned endpoints allocate devices in their constructors. Initialize only
// registers cleanup, so it is safe before a box has its own lifecycle scope. Keep
// the original endpoint: runtime Initialize must still register cleanup in its
// scope, and routing depends on concrete types and optional endpoint interfaces.
// Device Close and cancellation are idempotent when both scopes are closed.
func registerConstructorCleanup(scope *adapter.Scope, name, kind string, endpoint adapter.Endpoint) error {
	switch kind {
	case C.TypeWireGuard, C.TypeOpenConnect, C.TypeOpenVPNClient, C.TypeOpenVPNServer:
		return scope.Start(name, endpoint, adapter.StartStateInitialize)
	default:
		return nil
	}
}
