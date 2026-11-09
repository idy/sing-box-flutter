// SPDX-License-Identifier: GPL-3.0-or-later
package wgc

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	wireguard "github.com/idy/wgc"
	C "github.com/sagernet/sing-box/constant"
)

type transportSlot struct {
	wireguard.Transport
	cancel context.CancelFunc
}

func (s *transportSlot) Close() error { s.cancel(); return s.Transport.Close() }

// The WGC connection keeps one transport identity while physical sockets change.
// Closing the old socket wakes its blocked reader without closing the WGC session.
type rebindTransport struct {
	mu      sync.RWMutex
	active  *transportSlot
	changed chan struct{}
	request func()
}

func (t *rebindTransport) snapshot() (*transportSlot, <-chan struct{}) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.active, t.changed
}
func (t *rebindTransport) replace(next *transportSlot) bool {
	t.mu.Lock()
	previous := t.active
	if previous == nil {
		t.mu.Unlock()
		_ = next.Close()
		return false
	}
	t.active = next
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
	_ = previous.Close()
	return true
}
func (t *rebindTransport) ReadFrom(data []byte) (int, netip.AddrPort, error) {
	for {
		current, changed := t.snapshot()
		if current == nil {
			return 0, netip.AddrPort{}, net.ErrClosed
		}
		n, from, err := current.ReadFrom(data)
		if err == nil {
			return n, from, nil
		}
		next, _ := t.snapshot()
		if next == nil {
			return 0, netip.AddrPort{}, net.ErrClosed
		}
		if next == current {
			t.request()
			<-changed
		}
	}
}
func (t *rebindTransport) WriteTo(data []byte, to netip.AddrPort) (int, error) {
	for {
		current, _ := t.snapshot()
		if current == nil {
			return 0, net.ErrClosed
		}
		n, err := current.WriteTo(data, to)
		if err == nil {
			return n, nil
		}
		next, _ := t.snapshot()
		if next == nil {
			return 0, net.ErrClosed
		}
		if next == current {
			t.request()
			return n, err
		}
	}
}
func (t *rebindTransport) Close() error {
	t.mu.Lock()
	previous := t.active
	t.active = nil
	if previous != nil {
		close(t.changed)
	}
	t.mu.Unlock()
	if previous != nil {
		return previous.Close()
	}
	return nil
}

func (e *Endpoint) openTransport(ctx context.Context) (*transportSlot, error) {
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(C.StartTimeout, cancel)
	transport, err := e.factory(ctx, e.options.PeerEndpoint, e.options.TransportOptions)
	expired := !timer.Stop()
	if expired || err != nil || transport == nil || ctx.Err() != nil {
		cancel()
		if transport != nil {
			_ = transport.Close()
		}
		return nil, errors.New("WGC transport startup failed")
	}
	return &transportSlot{transport, cancel}, nil
}

func (e *Endpoint) InterfaceUpdated(context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.updates == nil {
		return
	}
	select {
	case e.updates <- struct{}{}:
	default:
	}
}

func (e *Endpoint) watchInterfaces(ctx context.Context, transport *rebindTransport, conn *wireguard.Conn) {
	defer e.rebinders.Done()
	var lastAttempt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.updates:
		}
		// Also bound retries caused by failed reads/writes while a path is offline.
		if delay := time.Until(lastAttempt.Add(time.Second)); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		lastAttempt = time.Now()
		next, err := e.openTransport(ctx)
		if err != nil {
			e.InterfaceUpdated(ctx)
			continue
		}
		if !transport.replace(next) {
			return
		}
		// Announce the new local UDP endpoint without replacing crypto/session state.
		_, _, _ = conn.WriteTo(nil, e.peerKey)
	}
}
