package core

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"sync"

	"github.com/idy/sing-box-flutter/core/transport"
	wgc "github.com/idy/wgc"
	sg "github.com/sagernet/sing-box/transport/wireguard"
	M "github.com/sagernet/sing/common/metadata"
)

type peerDevice struct {
	device sg.Device
	key    wgc.Key
}
type SessionOptions struct {
	Registry *transport.Registry
	// PeerConfigLookup is an additional admission policy for declared peers.
	// Returning nil or an error denies that peer. It runs at peer admission,
	// not for each packet. The configured endpoint remains pinned by default.
	PeerConfigLookup func(wgc.Key) (*wgc.PeerConfig, error)
}

type Session struct {
	conn      *wgc.Conn
	meter     *meteredTransport
	peers     map[string]*peerDevice
	keys      map[wgc.Key]*peerDevice
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	once      sync.Once
	senders   sync.WaitGroup
	receiver  sync.WaitGroup
	mu        sync.Mutex
	err       error
	publicKey string
}

type Status struct {
	State           string `json:"state"`
	PublicKey       string `json:"public_key"`
	BytesSent       uint64 `json:"bytes_sent"`
	BytesReceived   uint64 `json:"bytes_received"`
	PacketsSent     uint64 `json:"packets_sent"`
	PacketsReceived uint64 `json:"packets_received"`
	CoreVersion     string `json:"core_version"`
}

// NewSession opens the encrypted transport and userspace peer stacks. It does
// not create an OS TUN, change host routes, or perform host DNS resolution.
func NewSession(ctx context.Context, config Config, options SessionOptions) (*Session, error) {
	if runtime.GOOS == "android" && !transport.HasSocketProtector(ctx) {
		return nil, transport.ErrSocketProtectorRequired
	}
	config, err := NormalizeConfig(config)
	if err != nil {
		return nil, err
	}
	registry := options.Registry
	if registry == nil {
		registry = transport.Default
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	trans, err := registry.Open(ctx, config.Transport)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = trans.Close()
		return nil, err
	}
	private, _ := parseKey(config.PrivateKey)
	endpoints := make(map[wgc.Key]netip.AddrPort)
	for _, peer := range config.Peers {
		key, _ := parseKey(peer.PublicKey)
		endpoints[key], _ = numericAddress(peer.Endpoint)
	}
	meter := &meteredTransport{Transport: trans}
	conn, err := wgc.NewConn(meter, wgc.Config{
		StaticPrivateKey: &private, DisableStats: true, SharedWriteQueue: true, MaxPeers: len(endpoints), ReadQueueSize: 128, WriteQueueSize: 128,
		PeerConfigLookup: func(key wgc.Key) (*wgc.PeerConfig, error) {
			address, ok := endpoints[key]
			if !ok {
				return nil, errors.New("undeclared peer")
			}
			if options.PeerConfigLookup != nil {
				return options.PeerConfigLookup(key)
			}
			return &wgc.PeerConfig{Addr: address, DisableAddrRoaming: true}, nil
		},
	})
	if err != nil {
		_ = trans.Close()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	session := &Session{conn: conn, meter: meter, peers: make(map[string]*peerDevice), keys: make(map[wgc.Key]*peerDevice), ctx: runCtx, cancel: cancel, done: make(chan struct{}), publicKey: conn.PublicKey().String()}
	addresses := make([]netip.Prefix, 0, len(config.Addresses))
	for _, address := range config.Addresses {
		prefix, _ := netip.ParsePrefix(address)
		addresses = append(addresses, prefix)
	}
	for _, peer := range config.Peers {
		if err := runCtx.Err(); err != nil {
			_ = session.Close()
			return nil, err
		}
		device, err := sg.NewDevice(sg.DeviceOptions{Context: runCtx, MTU: config.MTU, Address: addresses})
		if err != nil {
			_ = session.Close()
			return nil, err
		}
		key, _ := parseKey(peer.PublicKey)
		entry := &peerDevice{device: device, key: key}
		session.peers[peer.ID] = entry
		session.keys[key] = entry
		if err := device.Start(); err != nil {
			_ = session.Close()
			return nil, err
		}
	}
	for _, peer := range session.peers {
		session.senders.Add(1)
		go session.send(peer, int(config.MTU))
	}
	session.receiver.Add(1)
	go session.receive()
	go func() { <-runCtx.Done(); _ = session.Close() }()
	return session, nil
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	if s.err == nil && s.ctx.Err() == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.cancel()
}
func (s *Session) send(peer *peerDevice, mtu int) {
	defer s.senders.Done()
	buffers, sizes := [][]byte{make([]byte, mtu)}, []int{0}
	for {
		count, err := peer.device.Read(buffers, sizes, 0)
		if err != nil {
			if s.ctx.Err() == nil {
				s.fail(err)
			}
			return
		}
		// Keep draining stack output during shutdown: an in-flight inbound
		// injection may need room for its response before the device can close.
		if count > 0 && s.ctx.Err() == nil {
			// Admission denial and blocking apply only to this peer; later packets retry.
			if _, _, err := s.conn.WriteTo(buffers[0][:sizes[0]], peer.key); err != nil && !errors.Is(err, wgc.ErrPeerNotFound) && !errors.Is(err, wgc.ErrPeerBlocked) {
				s.fail(err)
			}
		}
	}
}
func (s *Session) receive() {
	defer s.receiver.Done()
	for packet := range s.conn.ReadQueue() {
		if peer := s.keys[packet.From().StaticPubKey()]; peer != nil && s.ctx.Err() == nil {
			// The pinned sing-box stackDevice.Write synchronously copies input
			// into gVisor-owned buffers via buffer.MakeWithData. Keep the WGC
			// packet alive until Write returns, then release its pooled storage.
			if _, err := peer.device.Write([][]byte{packet.Data}, 0); err != nil && s.ctx.Err() == nil {
				s.fail(errors.New("WGC peer stack receive failed"))
			}
		}
		packet.Release()
	}
	if s.ctx.Err() == nil {
		s.fail(net.ErrClosed)
	}
}
func (s *Session) DialPeer(ctx context.Context, id, network, address string) (net.Conn, error) {
	if s.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	peer := s.peers[id]
	if peer == nil {
		return nil, errors.New("undeclared peer")
	}
	remote, err := numericAddress(address)
	if err != nil {
		return nil, err
	}
	operationCtx, cleanup := s.operationContext(ctx)
	defer cleanup()
	conn, err := peer.device.DialContext(operationCtx, network, M.Socksaddr{Addr: remote.Addr(), Port: remote.Port()})
	if stopped := s.operationError(ctx); stopped != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, stopped
	}
	return conn, err
}

// Link caller cancellation/deadlines to the session lifetime. Do not hold a
// session lock while dialing: Close must be able to cancel a blocked dial.
func (s *Session) operationContext(ctx context.Context) (context.Context, func()) {
	operationCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	return operationCtx, func() { stop(); cancel() }
}

func (s *Session) operationError(ctx context.Context) error {
	if s.ctx.Err() != nil {
		return net.ErrClosed
	}
	return ctx.Err()
}

func (s *Session) listenPeer(ctx context.Context, id string, remote M.Socksaddr) (net.PacketConn, error) {
	if s.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	peer := s.peers[id]
	if peer == nil {
		return nil, errors.New("undeclared peer")
	}
	operationCtx, cleanup := s.operationContext(ctx)
	defer cleanup()
	conn, err := peer.device.ListenPacket(operationCtx, remote)
	if stopped := s.operationError(ctx); stopped != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, stopped
	}
	return conn, err
}
func (s *Session) Status() Status {
	status := Status{
		State:           "running",
		PublicKey:       s.publicKey,
		BytesSent:       s.meter.sentBytes.Load(),
		BytesReceived:   s.meter.receivedBytes.Load(),
		PacketsSent:     s.meter.sentPackets.Load(),
		PacketsReceived: s.meter.receivedPackets.Load(),
		CoreVersion:     SingBoxVersion,
	}
	if s.ctx.Err() != nil {
		status.State = "stopped"
	}
	return status
}
func (s *Session) Done() <-chan struct{} { return s.done }
func (s *Session) Err() error            { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *Session) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.conn.Close()
		// Join all inbound injections before gVisor detaches its dispatcher.
		// Senders continue draining responses until device.Close unblocks Read.
		s.receiver.Wait()
		for _, peer := range s.peers {
			_ = peer.device.Close()
		}
		s.senders.Wait()
		close(s.done)
	})
	return nil
}
