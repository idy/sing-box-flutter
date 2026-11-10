// SPDX-License-Identifier: GPL-3.0-or-later
//go:build windows

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit opt-in keeps ordinary configuration tests free of driver changes.
// Hosted Windows CI enables this to exercise the actual embedded Wintun driver.
func TestWintunAdapterLifecycle(t *testing.T) {
	if os.Getenv("SING_BOX_FLUTTER_WINTUN_SMOKE") != "1" {
		t.Skip("set SING_BOX_FLUTTER_WINTUN_SMOKE=1 to test the real Wintun adapter")
	}
	if !isElevated() {
		t.Fatal("the Wintun smoke test requires an elevated Windows process")
	}
	working := t.TempDir()
	for _, directory := range []string{working, filepath.Join(working, "cache"), filepath.Join(working, "tmp")} {
		if err := privateDirectory(directory); err != nil {
			t.Fatalf("could not prepare smoke-test storage: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := fmt.Sprintf("sbf-ci-%x", time.Now().UnixNano())
	// The adapter owns only a benchmark subnet. No default route
	// or system DNS policy is changed, so the runner retains its normal network.
	config := fmt.Sprintf(`{
		"dns":{"servers":[{"type":"udp","tag":"dns","server":"1.1.1.1"}],"final":"dns"},
		"inbounds":[{"type":"tun","interface_name":%q,"address":["198.18.255.1/30"],"auto_route":false,"stack":"system","mtu":1500}],
		"outbounds":[{"type":"direct","tag":"direct"}],
		"route":{"auto_detect_interface":false,"final":"direct"}
	}`, name)
	instance, err := (boxBackend{}).New(ctx, config, working)
	if err != nil {
		t.Fatalf("could not construct the native smoke-test core: %v", err)
	}
	t.Cleanup(func() {
		if err := instance.Close(); err != nil {
			t.Errorf("could not clean up the native smoke-test core: %v", err)
		}
	})
	if err := instance.Start(); err != nil {
		t.Fatalf("could not start the native Wintun core: %v", err)
	}
	waitForSmokeAdapter(t, ctx, name, true)
	if err := instance.Close(); err != nil {
		t.Fatalf("could not stop the native Wintun core: %v", err)
	}
	waitForSmokeAdapter(t, ctx, name, false)
}

func waitForSmokeAdapter(t *testing.T, parent context.Context, name string, present bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		interfaces, err := net.Interfaces()
		if err != nil {
			t.Fatalf("could not enumerate Windows network interfaces: %v", err)
		}
		found := false
		for _, current := range interfaces {
			if current.Name == name {
				found = true
				break
			}
		}
		if found == present {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Wintun adapter presence did not become %v before the deadline", present)
		case <-ticker.C:
		}
	}
}
