// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime/pprof"
	"slices"
	"strings"
	"testing"
	"time"
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
		{"wireguard", `{"type":"wireguard","tag":"vpn","address":["10.0.0.2/32"],"private_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`},
		{"openconnect", `{"type":"openconnect","tag":"vpn","server":"https://192.0.2.1"}`},
		{"openvpn-client", fmt.Sprintf(`{"type":"openvpn-client","tag":"vpn","mode":"static_key","cipher":"AES-256-CBC","server":"192.0.2.1","server_port":1194,"address":["10.8.0.2/24"],"peer_address":"10.8.0.1","static_key":%s}`, keyJSON)},
		{"openvpn-server", fmt.Sprintf(`{"type":"openvpn-server","tag":"vpn","mode":"static_key","cipher":"AES-256-CBC","network":"tcp","listen":"127.0.0.1","listen_port":1194,"address":["10.8.0.1/24"],"peer_address":"10.8.0.2","static_key":%s}`, keyJSON)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if !slices.Contains(selected["endpoints"], tc.kind) {
				t.Skip("endpoint not selected in this build")
			}
			before, _ := validationTCPWorkers()
			for range 4 {
				content := `{"endpoints":[` + tc.endpoint + `],"route":{"final":"vpn"}}`
				if err := CheckConfig(content); err != nil {
					t.Fatal(err)
				}
				requireValidationWorkers(t, before)
			}
			// Duplicate tags fail after both constructors allocate their devices,
			// when box.New returns no box whose Close could free either stack.
			content := `{"endpoints":[` + tc.endpoint + `,` + tc.endpoint + `],"route":{"final":"vpn"}}`
			if err := CheckConfig(content); err == nil {
				t.Fatal("duplicate endpoint tags unexpectedly succeeded")
			}
			requireValidationWorkers(t, before)
		})
	}
}
