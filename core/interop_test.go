//go:build with_gvisor

package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/wireguard-go/conn"
	"github.com/sagernet/wireguard-go/device"
	wgTun "github.com/sagernet/wireguard-go/tun"
)

type memoryTun struct {
	outbound, received chan []byte
	events             chan wgTun.Event
	done               chan struct{}
	once               sync.Once
}

func (t *memoryTun) File() *os.File             { return nil }
func (t *memoryTun) Name() (string, error)      { return "wgc-endpoint-interop", nil }
func (t *memoryTun) MTU() (int, error)          { return 1280, nil }
func (t *memoryTun) BatchSize() int             { return 1 }
func (t *memoryTun) Events() <-chan wgTun.Event { return t.events }
func (t *memoryTun) Close() error               { t.once.Do(func() { close(t.done); close(t.events) }); return nil }
func (t *memoryTun) Read(buffers [][]byte, sizes []int, offset int) (int, error) {
	select {
	case <-t.done:
		return 0, net.ErrClosed
	case packet := <-t.outbound:
		sizes[0] = copy(buffers[0][offset:], packet)
		return 1, nil
	}
}
func (t *memoryTun) Write(buffers [][]byte, offset int) (int, error) {
	for index, packet := range buffers {
		select {
		case <-t.done:
			return index, net.ErrClosed
		case t.received <- append([]byte(nil), packet[offset:]...):
		}
	}
	return len(buffers), nil
}

// The server uses the stock wireguard-go protocol implementation, not WGC.
// Its in-memory TUN echoes a UDP IP packet, without touching host routing.
func TestStockWireGuardInteroperability(t *testing.T) {
	clientPrivate, clientPublic := testKeys(t)
	serverPrivate, serverPublic := testKeys(t)
	serverKey, _ := parseKey(serverPrivate)
	clientKey, _ := parseKey(clientPublic)
	tun := &memoryTun{outbound: make(chan []byte, 8), received: make(chan []byte, 8), events: make(chan wgTun.Event, 1), done: make(chan struct{})}
	server := device.NewDevice(context.Background(), tun, conn.NewDefaultBind(nil), device.NewLogger(device.LogLevelSilent, ""), 1)
	defer server.Close()
	if err := server.IpcSet(fmt.Sprintf("private_key=%x\nlisten_port=0\npublic_key=%x\nallowed_ip=10.7.0.2/32\n", serverKey[:], clientKey[:])); err != nil {
		t.Fatal(err)
	}
	if err := server.Up(); err != nil {
		t.Fatal(err)
	}
	ipc, err := server.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	var port string
	for _, line := range strings.Split(ipc, "\n") {
		if strings.HasPrefix(line, "listen_port=") {
			port = strings.TrimPrefix(line, "listen_port=")
		}
	}
	if port == "" || port == "0" {
		t.Fatal("server socket is not bound")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	session, err := NewSession(ctx, Config{Version: 1, PrivateKey: clientPrivate, Addresses: []string{"10.7.0.2/32"}, Peers: []Peer{{ID: "stock", PublicKey: serverPublic, Endpoint: net.JoinHostPort("127.0.0.1", port)}}}, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	udp, err := session.DialPeer(ctx, "stock", "udp", "10.7.0.1:8081")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	var packet []byte
	for attempt := 0; attempt < 8 && packet == nil; attempt++ {
		if _, err := udp.Write([]byte("stock-wireguard")); err != nil {
			t.Fatal(err)
		}
		select {
		case packet = <-tun.received:
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if len(packet) < 28 || packet[0]>>4 != 4 || packet[9] != 17 {
		t.Fatal("stock peer did not receive an IPv4 UDP packet")
	}
	headerLength := int(packet[0]&15) * 4
	if !bytes.Equal(packet[headerLength+8:], []byte("stock-wireguard")) {
		t.Fatal("payload changed on the wire")
	}
	reply := append([]byte(nil), packet...)
	copy(reply[12:16], packet[16:20])
	copy(reply[16:20], packet[12:16])
	copy(reply[headerLength:headerLength+2], packet[headerLength+2:headerLength+4])
	copy(reply[headerLength+2:headerLength+4], packet[headerLength:headerLength+2])
	reply[headerLength+6], reply[headerLength+7] = 0, 0
	reply[10], reply[11] = 0, 0
	var sum uint32
	for index := 0; index < headerLength; index += 2 {
		sum += uint32(binary.BigEndian.Uint16(reply[index : index+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(reply[10:12], ^uint16(sum))
	tun.outbound <- reply
	_ = udp.SetReadDeadline(time.Now().Add(3 * time.Second))
	output := make([]byte, 64)
	n, err := udp.Read(output)
	if err != nil || string(output[:n]) != "stock-wireguard" {
		t.Fatalf("stock WireGuard round trip failed: %v", err)
	}
}
