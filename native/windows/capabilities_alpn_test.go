// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"testing"

	"github.com/sagernet/sing-box/option"
)

func TestTrojanALPNDictionaryCapabilityCollisions(t *testing.T) {
	testTrojanALPNDictionaryCapabilities(t, func(ctx context.Context, content string) (option.Options, error) {
		return parseConfig(ctx, content, false)
	})
}
