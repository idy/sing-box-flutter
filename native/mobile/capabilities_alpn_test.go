// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import "testing"

func TestTrojanALPNDictionaryCapabilityCollisions(t *testing.T) {
	testTrojanALPNDictionaryCapabilities(t, parseConfig)
}
