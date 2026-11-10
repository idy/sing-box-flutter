// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	boxservice "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing-box/protocol/trojan"
)

func testTrojanALPNDictionaryCapabilities(t *testing.T, parse func(context.Context, string) (option.Options, error)) {
	t.Helper()
	// These are application ALPN strings, including case aliases of capability
	// fields. They neither configure stream transports nor select TLS features.
	for _, alpn := range []string{"transport", "tls", "certificate_provider", "Transport", "TLS", "Certificate_Provider"} {
		t.Run(alpn, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			inbounds := inbound.NewRegistry()
			trojan.RegisterInbound(inbounds)
			outbounds := outbound.NewRegistry()
			direct.RegisterOutbound(outbounds)
			ctx = box.Context(ctx, inbounds, outbounds, endpoint.NewRegistry(), dns.NewTransportRegistry(), boxservice.NewRegistry(), certificate.NewRegistry())
			name := strconv.Quote(alpn)
			content := `{"inbounds":[{"type":"trojan","tag":"trojan","listen":"127.0.0.1","users":[{"password":"fixture"}],"TLS":{"enabled":true,"insecure":true,"alpn":[` + name + `]},"Fallback_For_ALPN":{` + name + `:{"Server":"127.0.0.1","Server_Port":8443}}}],"outbounds":[{"type":"direct"}],"log":{"disabled":true}}`
			options, err := parse(ctx, content)
			if err != nil {
				t.Fatalf("valid Trojan ALPN dictionary rejected: %v", err)
			}
			instance, err := box.New(box.Options{Context: ctx, Options: options})
			if err != nil {
				t.Fatalf("native box rejected the accepted dictionary: %v", err)
			}
			defer instance.Close()
			incoming, ok := instance.Inbound().Get("trojan")
			if !ok {
				t.Fatal("native Trojan inbound missing")
			}
			serverSide, clientSide := net.Pipe()
			defer serverSide.Close()
			defer clientSide.Close()
			deadline, _ := ctx.Deadline()
			_ = serverSide.SetDeadline(deadline)
			_ = clientSide.SetDeadline(deadline)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				incoming.(adapter.TCPInjectableInbound).NewConnection(ctx, serverSide, adapter.InboundContext{}, nil)
			}()
			client := tls.Client(clientSide, &tls.Config{
				InsecureSkipVerify: true, // Native insecure mode generates a test certificate.
				NextProtos:         []string{alpn},
				MaxVersion:         tls.VersionTLS12,
			})
			if err := client.HandshakeContext(ctx); err != nil {
				t.Fatalf("native Trojan TLS handshake failed: %v", err)
			}
			if client.ConnectionState().NegotiatedProtocol != alpn {
				t.Fatal("native Trojan did not negotiate the application ALPN")
			}
			_ = clientSide.Close()
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("native Trojan connection did not close")
			}
		})
	}
}
