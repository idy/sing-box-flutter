// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"os"
	"strings"
	"testing"
)

func TestCoreProcessIdentityMatchesProcEntry(t *testing.T) {
	pid, start, err := coreProcessIdentity()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	foundPID, foundStart, err := parseProcessIdentity(string(content))
	if err != nil || foundPID != pid || foundStart != start {
		t.Fatalf("handshake identity does not locate this worker: %v", err)
	}
}

func TestProcessIdentityHandlesCommAndRejectsInvalidFields(t *testing.T) {
	stat := "42 (worker (name) with ) spaces) D " + strings.Repeat("0 ", 18) + "12345 0 0\n"
	pid, start, err := parseProcessIdentity(stat)
	if err != nil || pid != "42" || start != "12345" {
		t.Fatalf("valid proc identity was rejected: %q %q %v", pid, start, err)
	}
	for _, invalid := range []string{
		"", "42 no comm", "42 (worker) R 0", strings.Replace(stat, "42", "-1", 1),
		strings.Replace(stat, "12345", "private-invalid", 1),
		strings.Replace(stat, "12345", "012345", 1),
	} {
		if _, _, err := parseProcessIdentity(invalid); err != errProcessIdentity {
			t.Fatalf("invalid identity must fail with a sanitized error: %v", err)
		}
	}
}
