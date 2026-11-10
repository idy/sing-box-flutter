// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime/pprof"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/option"
)

func validationTCPWorkers() (int, string) {
	var trace bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&trace, 2)
	text := trace.String()
	return strings.Count(text, "github.com/sagernet/gvisor/pkg/tcpip/transport/tcp.(*processor).start("), text
}

func requireValidationWorkers(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, trace := validationTCPWorkers()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("validation retained gVisor TCP workers: got %d, want %d\n%s", got, want, trace)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCheckConfigClosesConstructorStacks(t *testing.T) {
	root := t.TempDir()
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
	if err := Setup(&SetupOptions{BasePath: root, WorkingPath: root, TempPath: root}); err != nil {
		t.Fatal(err)
	}
	key := "-----BEGIN OpenVPN Static key V1-----\n" + strings.Repeat("0123456789abcdef", 32) + "\n-----END OpenVPN Static key V1-----"
	keyJSON, _ := json.Marshal(key)
	selected := compiledCapabilities(t)
	for _, tc := range []struct {
		kind, endpoint string
	}{
		{"openconnect", `{"type":"openconnect","tag":"vpn","server":"https://192.0.2.1"}`},
		{"openvpn-client", fmt.Sprintf(`{"type":"openvpn-client","tag":"vpn","mode":"static_key","cipher":"AES-256-CBC","server":"192.0.2.1","server_port":1194,"address":["10.8.0.2/24"],"peer_address":"10.8.0.1","static_key":%s}`, keyJSON)},
		{"openvpn-server", fmt.Sprintf(`{"type":"openvpn-server","tag":"vpn","mode":"static_key","cipher":"AES-256-CBC","network":"tcp","listen":"127.0.0.1","listen_port":1194,"address":["10.8.0.1/24"],"peer_address":"10.8.0.2","static_key":%s}`, keyJSON)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if !slices.Contains(selected["endpoints"], tc.kind) {
				t.Skip("endpoint not selected in this build")
			}
			before, _ := validationTCPWorkers()
			content := `{"endpoints":[` + tc.endpoint + `],"route":{"final":"vpn"}}`
			for range 4 {
				if err := CheckConfig(content); err != nil {
					t.Fatal(err)
				}
				requireValidationWorkers(t, before)
			}
			// Allocating profiles retain disposable validation; preparation must
			// never retain its closed/stub-context box for runtime activation.
			constructors := 0
			server := &CommandServer{standalone: true, newEngine: func(ctx context.Context, _ PlatformInterface, _ option.Options) (mobileEngine, error) {
				constructors++
				return &fakeMobileEngine{ctx: ctx}, nil
			}}
			if _, err := server.PrepareService(content, nil); err != nil {
				t.Fatal(err)
			}
			if constructors != 0 || server.prepared == nil || server.prepared.engine != nil || server.prepared.options == nil {
				t.Fatal("allocating validation engine was retained")
			}
			requireValidationWorkers(t, before)
			if err := server.ActivateService(); err != nil || constructors != 1 {
				t.Fatal("fallback activation did not construct a fresh runtime")
			}
			server.Close()
			if !slices.Contains(selected["endpoints"], "openconnect") {
				return
			}
			// The first endpoint must be freed even when box.New returns no box.
			content = `{"endpoints":[` + tc.endpoint + `,{"type":"openconnect","tag":"invalid","server":"https://["}],"route":{"final":"vpn"}}`
			if err := CheckConfig(content); err == nil {
				t.Fatal("later constructor unexpectedly succeeded")
			}
			requireValidationWorkers(t, before)
			failed := &CommandServer{standalone: true, newEngine: server.newEngine}
			if _, err := failed.PrepareService(content, nil); err == nil {
				t.Fatal("preparation retained a partially constructed validation box")
			}
			failed.Close()
			requireValidationWorkers(t, before)
		})
	}
}
