// SPDX-License-Identifier: GPL-3.0-or-later
//go:build linux && !android

package libbox

import (
	"context"
	"slices"
	"testing"
	"time"
)

func setupDesktopTest(t *testing.T) {
	t.Helper()
	setupAccess.RLock()
	base, working, temp := sBasePath, sWorkingPath, sTempPath
	fixAndroidStack := sFixAndroidStack.Load()
	setupAccess.RUnlock()
	t.Cleanup(func() {
		setupAccess.Lock()
		sBasePath, sWorkingPath, sTempPath = base, working, temp
		sFixAndroidStack.Store(fixAndroidStack)
		setupAccess.Unlock()
	})
	root := t.TempDir()
	if err := Setup(&SetupOptions{BasePath: root, WorkingPath: root, TempPath: root}); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopLogPolicyMatchesConstruction(t *testing.T) {
	if !slices.Contains(compiledCapabilities(t)["outbounds"], "direct") {
		t.Skip("requires direct")
	}
	setupDesktopTest(t)
	for _, output := range []string{"stdout", "stderr"} {
		content := `{"log":{"output":"` + output + `"},"outbounds":[{"type":"direct"}]}`
		if err := CheckDesktopConfig(content); err != nil {
			t.Fatalf("desktop validation rejected discarded logs: %v", err)
		}
		if err := CheckConfig(content); err == nil {
			t.Fatal("mobile validation lost its private-log policy")
		}
		instance, err := NewDesktopService(context.Background(), content)
		if err != nil {
			t.Fatal(err)
		}
		if err = instance.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDesktopRetainsConstructorCleanupThroughStartup(t *testing.T) {
	selected := compiledCapabilities(t)
	if !slices.Contains(selected["endpoints"], "openconnect") {
		t.Skip("requires OpenConnect")
	}
	setupDesktopTest(t)
	endpoint := `{"type":"openconnect","tag":"vpn","server":"https://192.0.2.1","system":false}`
	before, _ := validationTCPWorkers()
	for range 3 {
		content := `{"endpoints":[` + endpoint + `,{"type":"openconnect","tag":"invalid","server":"https://["}],"route":{"final":"vpn"}}`
		if instance, err := NewDesktopService(context.Background(), content); err == nil || instance != nil {
			t.Fatal("later constructor unexpectedly succeeded")
		}
		requireValidationWorkers(t, before)
		content = `{"endpoints":[` + endpoint + `],"route":{"final":"missing"}}`
		instance, err := NewDesktopService(context.Background(), content)
		if err != nil {
			t.Fatal(err)
		}
		if err = instance.Start(); err == nil {
			t.Fatal("invalid final outbound unexpectedly started")
		}
		_ = instance.Close()
		_ = instance.Close()
		requireValidationWorkers(t, before)
	}
	if slices.Contains(selected["dns"], "local") {
		// Exercise normal endpoint Initialize as well as constructor ownership.
		// Both pinned cleanup registrations must be safe without endpoint proxies.
		instance, err := NewDesktopService(context.Background(), `{"endpoints":[`+endpoint+`],"route":{"final":"vpn"}}`)
		if err != nil {
			t.Fatal(err)
		}
		if err = instance.box.PreStart(); err != nil {
			_ = instance.Close()
			t.Fatal(err)
		}
		_ = instance.Close()
		requireValidationWorkers(t, before)
	}
}

func TestDesktopContextTracksItsParent(t *testing.T) {
	if !slices.Contains(compiledCapabilities(t)["outbounds"], "direct") {
		t.Skip("requires direct")
	}
	setupDesktopTest(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	instance, err := NewDesktopService(parent, `{"outbounds":[{"type":"direct"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	cancel()
	select {
	case <-instance.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("desktop engine did not receive parent cancellation")
	}
}
