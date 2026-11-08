package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	wgc "github.com/idy/wgc"
)

type testTransport struct{ closes atomic.Int32 }

func (t *testTransport) ReadFrom([]byte) (int, netip.AddrPort, error) {
	return 0, netip.AddrPort{}, errors.New("unused")
}
func (t *testTransport) WriteTo(p []byte, _ netip.AddrPort) (int, error) { return len(p), nil }
func (t *testTransport) Close() error                                    { t.closes.Add(1); return nil }

func TestWrapperFailureClosesBase(t *testing.T) {
	r := NewRegistry()
	base := &testTransport{}
	if err := r.RegisterFactory("test", func(context.Context, json.RawMessage) (wgc.Transport, error) { return base, nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterWrapper("broken", func(context.Context, wgc.Transport, json.RawMessage) (wgc.Transport, error) {
		return nil, errors.New("failed")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(context.Background(), Spec{Type: "broken", Base: &Spec{Type: "test"}}); err == nil {
		t.Fatal("failure accepted")
	}
	if base.closes.Load() != 1 {
		t.Fatal("base leaked or closed twice")
	}
}
func TestRegistrationAndValidationDoNotOpenNetwork(t *testing.T) {
	r := NewRegistry()
	var opened atomic.Bool
	factory := func(context.Context, json.RawMessage) (wgc.Transport, error) {
		opened.Store(true)
		return &testTransport{}, nil
	}
	if err := r.RegisterFactory("private-custom", factory); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterFactory("private-custom", factory); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	for _, name := range []string{"", "../bad", "Bad", "space name"} {
		if err := r.RegisterFactory(name, factory); err == nil {
			t.Fatal("invalid name accepted")
		}
	}
	if err := r.Validate(Spec{Type: "private-custom"}); err != nil {
		t.Fatal(err)
	}
	if opened.Load() {
		t.Fatal("validation opened a transport")
	}
	if _, err := r.Open(context.Background(), Spec{Type: "missing"}); err == nil {
		t.Fatal("unknown transport fell back")
	}
	if opened.Load() {
		t.Fatal("unknown transport opened a factory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Open(ctx, Spec{Type: "private-custom"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestZeroRegistryCustomUDPOptionsRemainOpaque(t *testing.T) {
	ctx := WithSocketProtector(context.Background(), func(int) error {
		panic("custom transport regression must not open sockets")
	})
	raw := json.RawMessage(`{"listen":{"custom_host":"private-listen-host.invalid"},"custom_option":true}`)
	for _, spec := range []Spec{
		{Options: raw},
		{Type: "udp", Options: raw},
		{Type: "passthrough", Base: &Spec{Type: "udp", Options: raw}},
	} {
		var registry Registry
		var opens, wraps int
		base := &testTransport{}
		if err := registry.RegisterFactory("udp", func(got context.Context, options json.RawMessage) (wgc.Transport, error) {
			opens++
			if got != ctx || string(options) != string(raw) {
				t.Fatal("custom UDP factory lost its context or private options")
			}
			return base, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := registry.RegisterWrapper("passthrough", func(_ context.Context, inner wgc.Transport, _ json.RawMessage) (wgc.Transport, error) {
			wraps++
			return inner, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := registry.Validate(spec); err != nil {
			t.Fatalf("custom UDP options rejected by preflight: %v", err)
		}
		if err := registry.ValidateContext(ctx, spec); err != nil {
			t.Fatalf("protected custom UDP options rejected by preflight: %v", err)
		}
		if opens != 0 || wraps != 0 {
			t.Fatal("preflight invoked a custom factory or wrapper")
		}
		conn, err := registry.Open(ctx, spec)
		if err != nil {
			t.Fatalf("custom UDP options rejected by open: %v", err)
		}
		wantWraps := 0
		if spec.Base != nil {
			wantWraps = 1
		}
		if conn != base || opens != 1 || wraps != wantWraps {
			t.Fatal("custom UDP factory/wrapper ownership changed")
		}
		if err := conn.Close(); err != nil || base.closes.Load() != 1 {
			t.Fatal("custom UDP transport did not close exactly once")
		}
	}
	if err := NewRegistry().ValidateContext(ctx, Spec{Type: "udp", Options: raw}); err == nil {
		t.Fatal("built-in UDP lost its options validator")
	}
}

func TestUDPListenPreflightParser(t *testing.T) {
	for _, test := range []struct {
		name    string
		raw     string
		address string
		valid   bool
	}{
		{"omitted", "", ":0", true},
		{"default", `{}`, ":0", true},
		{"empty listen", `{"listen":""}`, ":0", true},
		{"wildcard", `{"listen":":0"}`, ":0", true},
		{"IPv4 wildcard", `{"listen":"0.0.0.0:0"}`, "0.0.0.0:0", true},
		{"IPv6 wildcard", `{"listen":"[::]:0"}`, "[::]:0", true},
		{"IPv4 loopback", `{"listen":"127.0.0.1:0"}`, "127.0.0.1:0", true},
		{"IPv6 loopback", `{"listen":"[::1]:65535"}`, "[::1]:65535", true},
		{"IPv6 zone", `{"listen":"[fe80::1%test-interface]:0"}`, "[fe80::1%test-interface]:0", true},
		{"secret-unknown-field", `{"secret-unknown-field":"private-value"}`, "", false},
		{"wrong listen type", `{"listen":47}`, "", false},
		{"null listen", `{"listen":null}`, "", false},
		{"wrong options type", `["private-value"]`, "", false},
		{"null options", `null`, "", false},
		{"multiple values", `{} {}`, "", false},
		{"missing port", `{"listen":"127.0.0.1"}`, "", false},
		{"negative port", `{"listen":"127.0.0.1:-1"}`, "", false},
		{"overflow port", `{"listen":"127.0.0.1:65536"}`, "", false},
		{"named port", `{"listen":"127.0.0.1:private-value"}`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, numericOnly := range []bool{false, true} {
				address, err := parseUDPListen(json.RawMessage(test.raw), numericOnly)
				if (err == nil) != test.valid || address != test.address {
					t.Fatalf("UDP parser = %q, %v, want %q, valid=%v", address, err, test.address, test.valid)
				}
				if err != nil && (strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "secret-unknown-field")) {
					t.Fatal("UDP parser leaked private options")
				}
			}
		})
	}
	const hostname = `{"listen":"private-listen-host.invalid:0"}`
	if _, err := parseUDPListen(json.RawMessage(hostname), false); err != nil {
		t.Fatalf("legacy unprotected hostname rejected: %v", err)
	}
	if _, err := parseUDPListen(json.RawMessage(hostname), true); err == nil || strings.Contains(err.Error(), "private-listen-host") {
		t.Fatalf("protected hostname was accepted or leaked: %v", err)
	}
}

func TestProtectedUDPPreflightRejectsBeforeDNSOrSocket(t *testing.T) {
	var lookups, sockets, wrappers atomic.Int32
	resolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		lookups.Add(1)
		return nil, errors.New("DNS is forbidden in this test")
	}}
	t.Cleanup(func() { net.DefaultResolver = resolver })
	ctx := WithSocketProtector(context.Background(), func(int) error { sockets.Add(1); return nil })
	registry := NewRegistry()
	if err := registry.RegisterWrapper("passthrough", func(_ context.Context, base wgc.Transport, _ json.RawMessage) (wgc.Transport, error) {
		wrappers.Add(1)
		return base, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"listen":"private-listen-host.invalid:0"}`),
		json.RawMessage(`{"unknown-private-option":"private-value"}`),
		json.RawMessage(`{"listen":{"private-value":true}}`),
	} {
		for depth := range 4 {
			spec := Spec{Options: raw}
			for range depth {
				base := spec
				spec = Spec{Type: "passthrough", Base: &base}
			}
			validationErr := registry.ValidateContext(ctx, spec)
			conn, openErr := registry.Open(ctx, spec)
			if conn != nil {
				_ = conn.Close()
			}
			if validationErr == nil || openErr == nil || validationErr.Error() != openErr.Error() {
				t.Fatalf("preflight/open disagreed at depth %d: %v / %v", depth, validationErr, openErr)
			}
			if strings.Contains(openErr.Error(), "private-") || strings.Contains(openErr.Error(), "unknown-private-option") {
				t.Fatalf("UDP preflight leaked options: %v", openErr)
			}
		}
	}
	if lookups.Load() != 0 || sockets.Load() != 0 || wrappers.Load() != 0 {
		t.Fatalf("rejection crossed the network/factory boundary: DNS=%d sockets=%d wrappers=%d", lookups.Load(), sockets.Load(), wrappers.Load())
	}
	// An implicit UDP base under a wrapper must also validate offline.
	if err := registry.ValidateContext(ctx, Spec{Type: "passthrough"}); err != nil || wrappers.Load() != 0 {
		t.Fatalf("implicit UDP preflight opened or rejected a wrapper: %v", err)
	}
	if runtime.GOOS != "android" {
		if err := registry.Validate(Spec{Options: json.RawMessage(`{"listen":"private-listen-host.invalid:0"}`)}); err != nil {
			t.Fatalf("legacy unprotected preflight rejected a hostname: %v", err)
		}
	}
}

func TestDefaultUDPAndWrapperOwnership(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterWrapper("passthrough", func(_ context.Context, base wgc.Transport, _ json.RawMessage) (wgc.Transport, error) {
		return base, nil
	}); err != nil {
		t.Fatal(err)
	}
	conn, err := r.Open(context.Background(), Spec{Type: "passthrough"})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	nested := Spec{Type: "udp"}
	for i := 0; i < 20; i++ {
		next := nested
		nested = Spec{Type: "passthrough", Base: &next}
	}
	if err := r.Validate(nested); err == nil {
		t.Fatal("unbounded wrapper nesting accepted")
	}
	if err := r.Validate(Spec{Type: "udp", Base: &Spec{Type: "udp"}}); err == nil {
		t.Fatal("factory with base accepted")
	}
}
