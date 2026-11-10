// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"io"
	"strings"
	"testing"
)

func TestProtocolAcceptsFrameLimitIncludingNewline(t *testing.T) {
	h := newHarness(t, fakeBackend{})
	prefix := `{"id":1,"method":"getVersion","arguments":{"padding":"`
	suffix := "\"}}\n"
	frame := prefix + strings.Repeat("x", maxRequestBytes-len(prefix)-len(suffix)) + suffix
	if _, err := io.WriteString(h.input, frame); err != nil {
		t.Fatal(err)
	}
	if got := string(h.response(1, "").Result); got != `"test-core"` {
		t.Fatal("maximum-size request did not reach the helper")
	}
}
