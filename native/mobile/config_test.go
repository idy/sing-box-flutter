// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

func compiledCapabilities(t *testing.T) map[string][]string {
	t.Helper()
	var selected map[string][]string
	if err := stdjson.Unmarshal([]byte(selectedFeaturesJSON), &selected); err != nil {
		t.Fatal(err)
	}
	return selected
}

func TestParseConfigRejectsUnselectedImplicitDirect(t *testing.T) {
	if slices.Contains(compiledCapabilities(t)["outbounds"], "direct") {
		t.Skip("requires direct unselected")
	}
	ctx := mobileBaseContext(nil)
	for _, content := range []string{`{}`, `{"outbounds":[]}`, `{"route":{"final":""}}`} {
		if _, err := parseConfig(ctx, content); err == nil {
			t.Fatalf("unselected implicit direct accepted: %s", content)
		}
	}
	if _, err := parseConfig(ctx, `{"route":{"final":"explicit-endpoint"}}`); err != nil {
		t.Fatalf("explicit final was incorrectly treated as fallback: %v", err)
	}
}

func TestParseConfigAcceptsNativeCommentSyntax(t *testing.T) {
	if !slices.Contains(compiledCapabilities(t)["outbounds"], "direct") {
		t.Skip("requires direct selected")
	}
	// Comment text survives re-encoding inside protocol entries; the capability
	// gate must still read it like the engine does.
	content := "{\"outbounds\":[{\"type\":\"direct\", /* c */ \"tag\":\"d\"},], // c\n\"route\":{\"final\":\"d\"},}"
	if _, err := parseConfig(mobileBaseContext(nil), content); err != nil {
		t.Fatalf("commented configuration rejected: %v", err)
	}
}

func TestParseConfigRejectsEffectiveMixedCaseTransport(t *testing.T) {
	selected := compiledCapabilities(t)
	if !slices.Contains(selected["outbounds"], "vmess") || slices.Contains(selected["transports"], "ws") {
		t.Skip("requires vmess selected and websocket unselected")
	}
	ctx := mobileBaseContext(nil)
	content := `{"outbounds":[{"type":"vmess","tag":"proxy","server":"192.0.2.1","server_port":443,"uuid":"11111111-1111-4111-8111-111111111111","security":"auto","Transport":{"type":"ws","path":"/private-path"}}]}`
	// Establish that decoding succeeds and the old raw gate misses the feature.
	if err := checkSelectedCapabilities(content); err != nil {
		t.Fatal(err)
	}
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	transport := options.Outbounds[0].Options.(*option.VMessOutboundOptions).Transport
	if transport == nil || transport.Type != "ws" {
		t.Fatal("native decoder did not resolve the mixed-case transport")
	}
	if _, err = parseConfig(ctx, content); err == nil || strings.Contains(err.Error(), "private-path") {
		t.Fatalf("production parser accepted unselected effective transport: %v", err)
	}
}

func TestParseConfigRejectsEffectiveRepeatedECH(t *testing.T) {
	if slices.Contains(compiledCapabilities(t)["tls"], "ech") {
		t.Skip("requires ECH unselected")
	}
	ctx := mobileBaseContext(nil)
	content := `{"http_clients":[{"tag":"test","version":1,"tls":{"ech":{"enabled":true},"ech":{}}}],"route":{"final":"fixture"}}`
	if err := checkSelectedCapabilities(content); err != nil {
		t.Fatal(err)
	}
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if options.HTTPClients[0].TLS == nil || options.HTTPClients[0].TLS.ECH == nil || !options.HTTPClients[0].TLS.ECH.Enabled {
		t.Fatal("native decoder did not merge the repeated ECH object")
	}
	if _, err = parseConfig(ctx, content); err == nil {
		t.Fatal("production parser accepted unselected effective ECH")
	}
}

func TestParseConfigPreservesNativeObjectSemantics(t *testing.T) {
	ctx := mobileBaseContext(nil)
	// Explicit false overwrites true; an empty object instead merges fields.
	content := `{"HTTP_Clients":[{"tag":"test","version":1,"TLS":{"ech":{"enabled":true},"ech":{"enabled":false}}}],"route":{"final":"fixture"}}`
	options, err := parseConfig(ctx, content)
	if err != nil {
		t.Fatal(err)
	}
	if options.HTTPClients[0].TLS.ECH.Enabled {
		t.Fatal("explicit false was lost")
	}
	for _, invalid := range []string{`null`, `[]`, `{"unknown-private-field":true}`} {
		if _, err = parseConfig(ctx, invalid); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("invalid configuration accepted or exposed: %v", err)
		}
	}
}

type opaqueEndpointOptions struct {
	Transport        string          `json:"transport,omitempty"`
	TransportOptions json.RawMessage `json:"transport_options,omitempty"`
}

func TestParseConfigPreservesOpaqueApplicationOptions(t *testing.T) {
	if !slices.Contains(compiledCapabilities(t)["endpoints"], "wgc") {
		t.Skip("requires the application endpoint selected")
	}
	registry := endpoint.NewRegistry()
	endpoint.Register[opaqueEndpointOptions](registry, "wgc", func(context.Context, adapter.Router, log.ContextLogger, string, opaqueEndpointOptions) (adapter.Endpoint, error) {
		return nil, errors.New("parsing must not construct an endpoint")
	})
	ctx := service.ContextWith[option.EndpointOptionsRegistry](mobileBaseContext(nil), registry)
	content := `{"endpoints":[{"type":"wgc","tag":"vpn","transport":"app","transport_options":{"tls":"opaque","Transport":{"type":"private-protocol"},"ech":{"enabled":true}}}],"route":{"final":"vpn"}}`
	options, err := parseConfig(ctx, content)
	if err != nil {
		t.Fatal(err)
	}
	opaque := options.Endpoints[0].Options.(*opaqueEndpointOptions)
	if !strings.Contains(string(opaque.TransportOptions), "private-protocol") || opaque.Transport != "app" {
		t.Fatal("opaque application options were changed")
	}
}

type validationTestEndpoint struct {
	adapter.Endpoint
	stages []adapter.StartStage
	closed int
	err    error
}

func (e *validationTestEndpoint) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	e.stages = append(e.stages, stage)
	scope.Add(func() error { e.closed++; return nil })
	return e.err
}

type validationTestRegistry struct {
	adapter.EndpointRegistry
	endpoint adapter.Endpoint
	err      error
}

func (r *validationTestRegistry) Create(context.Context, adapter.Router, log.ContextLogger, string, string, any) (adapter.Endpoint, error) {
	return r.endpoint, r.err
}

func TestValidationRegistryInitializesOnlyKnownCleanupStages(t *testing.T) {
	for _, kind := range []string{C.TypeOpenConnect, C.TypeOpenVPNClient, C.TypeOpenVPNServer, "wgc"} {
		t.Run(kind, func(t *testing.T) {
			logger := log.NewNOPFactory().Logger()
			scope := adapter.NewScope(context.Background(), logger)
			defer scope.Close()
			endpoint := &validationTestEndpoint{}
			registry := &validationEndpointRegistry{EndpointRegistry: &validationTestRegistry{endpoint: endpoint}, cleanup: scope}
			got, err := registry.Create(context.Background(), nil, logger, "vpn", kind, nil)
			if err != nil || got != endpoint {
				t.Fatalf("create: %v", err)
			}
			if kind == "wgc" {
				if len(endpoint.stages) != 0 {
					t.Fatal("unrelated endpoint was initialized")
				}
			} else if !slices.Equal(endpoint.stages, []adapter.StartStage{adapter.StartStateInitialize}) {
				t.Fatalf("validation started endpoint stages: %v", endpoint.stages)
			}
			if err := scope.Close(); err != nil {
				t.Fatal(err)
			}
			wantClosed := len(endpoint.stages)
			if endpoint.closed != wantClosed {
				t.Fatalf("cleanup count=%d, want %d", endpoint.closed, wantClosed)
			}
		})
	}
}

func TestValidationRegistryClosesCleanupOnInitializeFailure(t *testing.T) {
	logger := log.NewNOPFactory().Logger()
	scope := adapter.NewScope(context.Background(), logger)
	defer scope.Close()
	failure := errors.New("initialization failed")
	endpoint := &validationTestEndpoint{err: failure}
	registry := &validationEndpointRegistry{EndpointRegistry: &validationTestRegistry{endpoint: endpoint}, cleanup: scope}
	if _, err := registry.Create(context.Background(), nil, logger, "vpn", C.TypeOpenConnect, nil); !errors.Is(err, failure) {
		t.Fatalf("initialize error lost: %v", err)
	}
	if err := scope.Close(); err != nil || endpoint.closed != 1 {
		t.Fatalf("failed initialization retained cleanup: %v, closed=%d", err, endpoint.closed)
	}
}
