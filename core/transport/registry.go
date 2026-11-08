// Package transport constructs WGC transports inside the native extension.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"runtime"
	"strconv"
	"sync"

	wgc "github.com/idy/wgc"
	udp "github.com/idy/wgc/transport"
)

// Spec selects a compiled-in transport. A wrapper may compose a base transport;
// an omitted base means standard UDP. Options are private to its implementation.
type Spec struct {
	Type    string          `json:"type"`
	Options json.RawMessage `json:"options,omitempty"`
	Base    *Spec           `json:"base,omitempty"`
}

// Factory constructs a transport at endpoint startup, never during validation.
// It must honor context cancellation. Every outer socket (including DNS,
// reconnects and auxiliary sockets) must use SocketControl or ProtectSocket.
// A transport must unblock ReadFrom/WriteTo when Close is called.
type Factory func(context.Context, json.RawMessage) (wgc.Transport, error)

// Wrapper follows the same cancellation/protection contract as Factory and owns
// its base on success. Validation treats extension options as opaque JSON.
type Wrapper func(context.Context, wgc.Transport, json.RawMessage) (wgc.Transport, error)

type entry struct {
	factory  Factory
	wrapper  Wrapper
	validate func(json.RawMessage, bool) error
}
type Registry struct {
	mu      sync.RWMutex
	entries map[string]entry
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var Default = NewRegistry()

func NewRegistry() *Registry {
	return &Registry{entries: map[string]entry{
		"udp": {
			factory: openUDP,
			validate: func(raw json.RawMessage, numericOnly bool) error {
				_, err := parseUDPListen(raw, numericOnly)
				return err
			},
		},
	}}
}

func (r *Registry) register(name string, value entry) error {
	if !validName.MatchString(name) || (value.factory == nil && value.wrapper == nil) {
		return errors.New("invalid transport registration")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]entry)
	}
	if _, exists := r.entries[name]; exists {
		return fmt.Errorf("transport %q already registered", name)
	}
	r.entries[name] = value
	return nil
}

func (r *Registry) RegisterFactory(name string, factory Factory) error {
	return r.register(name, entry{factory: factory})
}
func (r *Registry) RegisterWrapper(name string, wrapper Wrapper) error {
	return r.register(name, entry{wrapper: wrapper})
}
func RegisterFactory(name string, factory Factory) error {
	return Default.RegisterFactory(name, factory)
}
func RegisterWrapper(name string, wrapper Wrapper) error {
	return Default.RegisterWrapper(name, wrapper)
}

func (r *Registry) Validate(spec Spec) error {
	return r.ValidateContext(context.Background(), spec)
}

// ValidateContext checks registrations and built-in UDP options without opening
// sockets or resolving names. Android and protected contexts require numeric UDP
// listen hosts, since Go otherwise resolves them before the socket control runs.
func (r *Registry) ValidateContext(ctx context.Context, spec Spec) error {
	return r.validate(spec, 0, runtime.GOOS == "android" || HasSocketProtector(ctx))
}

func (r *Registry) validate(spec Spec, depth int, numericOnly bool) error {
	if depth > 8 {
		return errors.New("transport nesting exceeds eight wrappers")
	}
	if spec.Type == "" {
		spec.Type = "udp"
	}
	r.mu.RLock()
	value, found := r.entries[spec.Type]
	r.mu.RUnlock()
	if !found {
		return fmt.Errorf("transport %q is not compiled into this core", spec.Type)
	}
	if len(spec.Options) > 0 && (!json.Valid(spec.Options) || bytes.Equal(bytes.TrimSpace(spec.Options), []byte("null"))) {
		return errors.New("invalid transport options")
	}
	if value.wrapper != nil {
		base := Spec{Type: "udp"}
		if spec.Base != nil {
			base = *spec.Base
		}
		return r.validate(base, depth+1, numericOnly)
	}
	if spec.Base != nil {
		return errors.New("base is only valid for a transport wrapper")
	}
	if value.validate != nil {
		return value.validate(spec.Options, numericOnly)
	}
	return nil
}

// Open closes a wrapper's base if construction fails. On success the returned
// transport owns its base, and must close it when Close is called.
func (r *Registry) Open(ctx context.Context, spec Spec) (wgc.Transport, error) {
	if err := r.ValidateContext(ctx, spec); err != nil {
		return nil, err
	}
	return r.open(ctx, spec)
}
func (r *Registry) open(ctx context.Context, spec Spec) (wgc.Transport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Type == "" {
		spec.Type = "udp"
	}
	r.mu.RLock()
	value := r.entries[spec.Type]
	r.mu.RUnlock()
	options := spec.Options
	if len(options) == 0 {
		options = json.RawMessage(`{}`)
	}
	if value.wrapper == nil {
		result, err := value.factory(ctx, options)
		if err != nil {
			if result != nil {
				_ = result.Close()
			}
			return nil, err
		}
		if result == nil {
			return nil, errors.New("transport factory returned nil")
		}
		return result, nil
	}
	base := Spec{Type: "udp"}
	if spec.Base != nil {
		base = *spec.Base
	}
	inner, err := r.open(ctx, base)
	if err != nil {
		return nil, err
	}
	result, err := value.wrapper(ctx, inner, options)
	if err != nil || result == nil {
		_ = inner.Close()
		if err == nil {
			err = errors.New("transport wrapper returned nil")
		}
		return nil, err
	}
	return result, nil
}

func parseUDPListen(raw json.RawMessage, numericOnly bool) (string, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var options struct {
		Listen json.RawMessage `json:"listen,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || decoder.Decode(&options) != nil || decoder.Decode(new(any)) != io.EOF {
		return "", errors.New("invalid UDP options")
	}
	var address string
	if len(options.Listen) != 0 && (bytes.Equal(bytes.TrimSpace(options.Listen), []byte("null")) || json.Unmarshal(options.Listen, &address) != nil) {
		return "", errors.New("invalid UDP options")
	}
	if address == "" {
		address = ":0"
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", errors.New("invalid UDP listen address")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", errors.New("invalid UDP listen port")
	}
	if numericOnly && host != "" {
		if _, err := netip.ParseAddr(host); err != nil {
			return "", errors.New("UDP listen host must be numeric when socket protection is required")
		}
	}
	return address, nil
}

func openUDP(ctx context.Context, raw json.RawMessage) (wgc.Transport, error) {
	address, err := parseUDPListen(raw, runtime.GOOS == "android" || HasSocketProtector(ctx))
	if err != nil {
		return nil, err
	}
	listener := net.ListenConfig{Control: SocketControl(ctx)}
	conn, err := listener.ListenPacket(ctx, "udp", address)
	if err != nil {
		return nil, errors.New("cannot open UDP transport")
	}
	return udp.FromUDPConn(conn.(*net.UDPConn)), nil
}
