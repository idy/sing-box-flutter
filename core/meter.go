package core

import (
	wgc "github.com/idy/wgc"
	"net/netip"
	"sync/atomic"
)

// Counters live at the selected transport boundary. Keep WGC's optional peer
// statistics disabled: these atomic totals include encrypted datagrams and
// handshakes without maintaining duplicate accounting.
type meteredTransport struct {
	wgc.Transport
	sentBytes, receivedBytes, sentPackets, receivedPackets atomic.Uint64
}

func (m *meteredTransport) ReadFrom(packet []byte) (int, netip.AddrPort, error) {
	n, remote, err := m.Transport.ReadFrom(packet)
	if n > 0 {
		m.receivedBytes.Add(uint64(n))
	}
	if err == nil {
		m.receivedPackets.Add(1)
	}
	return n, remote, err
}
func (m *meteredTransport) WriteTo(packet []byte, remote netip.AddrPort) (int, error) {
	n, err := m.Transport.WriteTo(packet, remote)
	if n > 0 {
		m.sentBytes.Add(uint64(n))
	}
	if err == nil {
		m.sentPackets.Add(1)
	}
	return n, err
}
