// SPDX-License-Identifier: GPL-3.0-or-later
//go:build darwin

package libbox

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	tun "github.com/sagernet/sing-tun"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// FetchRIB is read-only and requires no elevation. Keep interface ownership as
// well as prefixes; gateway routes and scoped routes have different semantics.
func readStandaloneRoutes() ([]standaloneRoute, error) {
	rib, err := route.FetchRIB(syscall.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, err
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil, err
	}
	var prefixes []standaloneRoute
	for _, message := range messages {
		m, ok := message.(*route.RouteMessage)
		if !ok {
			continue
		}
		prefix, err := standaloneRoutePrefix(m)
		if err != nil {
			return nil, err
		}
		if prefix.IsValid() {
			prefixes = append(prefixes, standaloneRoute{destination: prefix,
				interfaceRoute: m.Flags&syscall.RTF_GATEWAY == 0, interfaceIndex: m.Index,
				scoped: m.Flags&syscall.RTF_IFSCOPE != 0})
		}
	}
	return prefixes, nil
}

func openStandaloneTun(options tun.Options) (tun.Tun, error) {
	return newStandaloneTun(options, tun.New, addStandaloneRoute)
}

func addStandaloneRoute(name string, destination netip.Prefix, gateway netip.Addr) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	return installStandaloneRoute(iface.Index, destination, gateway, writeStandaloneRoute, readStandaloneRoutes)
}

// Routing sockets broadcast replies to other sockets in this process too. Use
// distinct sequences so concurrent TUNs cannot consume each other's result.
var standaloneRouteSequence = uint32(time.Now().UnixNano())

func installStandaloneRoute(index int, destination netip.Prefix, gateway netip.Addr,
	write func(*route.RouteMessage) error, read func() ([]standaloneRoute, error),
) error {
	message := &route.RouteMessage{
		Type: syscall.RTM_ADD, Version: syscall.RTM_VERSION,
		Flags: syscall.RTF_UP | syscall.RTF_STATIC | syscall.RTF_GATEWAY,
		Index: index, ID: uintptr(os.Getpid()), Seq: int(atomic.AddUint32(&standaloneRouteSequence, 1) & 0x7fffffff),
	}
	if destination.Bits() == destination.Addr().BitLen() {
		message.Flags |= syscall.RTF_HOST
	}
	// Pin the gateway route to this newly created utun. Its descriptor lifetime
	// then owns cleanup, including routes installed before a later add fails.
	message.Addrs = make([]route.Addr, syscall.RTAX_IFP+1)
	message.Addrs[syscall.RTAX_IFP] = &route.LinkAddr{Index: index}
	if destination.Addr().Is4() {
		message.Addrs[syscall.RTAX_DST] = &route.Inet4Addr{IP: destination.Addr().As4()}
		message.Addrs[syscall.RTAX_NETMASK] = &route.Inet4Addr{IP: [4]byte(net.CIDRMask(destination.Bits(), 32))}
		message.Addrs[syscall.RTAX_GATEWAY] = &route.Inet4Addr{IP: gateway.As4()}
	} else {
		message.Addrs[syscall.RTAX_DST] = &route.Inet6Addr{IP: destination.Addr().As16()}
		message.Addrs[syscall.RTAX_NETMASK] = &route.Inet6Addr{IP: [16]byte(net.CIDRMask(destination.Bits(), 128))}
		message.Addrs[syscall.RTAX_GATEWAY] = &route.Inet6Addr{IP: gateway.As16()}
	}
	err := write(message)
	if !errors.Is(err, syscall.EEXIST) {
		return err
	}
	// Address assignment may already have installed a connected/host route.
	// Accept only an unscoped, directly attached route on our exact new utun;
	// another interface, gateway, scope or an unreadable table remains a failure.
	existing, readErr := read()
	if readErr != nil {
		return err
	}
	owned := false
	for _, installed := range existing {
		if installed.destination.Masked() != destination.Masked() || installed.scoped {
			continue
		}
		if !installed.interfaceRoute || installed.interfaceIndex != index {
			return err
		}
		owned = true
	}
	if owned {
		return nil
	}
	return err
}

func standaloneRouteReply(request *route.RouteMessage, messages []route.Message) (bool, error) {
	for _, message := range messages {
		if reply, ok := message.(*route.RouteMessage); ok && reply.Type == request.Type && reply.ID == request.ID && reply.Seq == request.Seq {
			return true, reply.Err
		}
	}
	return false, nil
}

func writeStandaloneRoute(request *route.RouteMessage) error {
	// This boundary deliberately supports only ADD. There is no deletion or
	// replacement fallback, including after EEXIST or an acknowledgement error.
	if request.Type != syscall.RTM_ADD {
		return os.ErrInvalid
	}
	data, err := request.Marshal()
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	unix.CloseOnExec(fd)
	timeout := unix.NsecToTimeval(int64(time.Second))
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		return err
	}
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &timeout); err != nil {
		return err
	}
	n, err := unix.Write(fd, data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	// Also inspect the kernel acknowledgement: a successful write by itself
	// must not hide an errno carried in the routing socket response.
	buffer := make([]byte, 64*1024)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		n, err = unix.Read(fd, buffer)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		messages, err := route.ParseRIB(route.RIBTypeRoute, buffer[:n])
		if err != nil {
			return err
		}
		if matched, err := standaloneRouteReply(request, messages); matched {
			return err
		}
	}
	return syscall.ETIMEDOUT
}

func standaloneRoutePrefix(m *route.RouteMessage) (netip.Prefix, error) {
	if len(m.Addrs) <= syscall.RTAX_DST {
		return netip.Prefix{}, errors.New("missing route destination")
	}
	var address netip.Addr
	var mask net.IPMask
	var netmask route.Addr
	if len(m.Addrs) > syscall.RTAX_NETMASK {
		netmask = m.Addrs[syscall.RTAX_NETMASK]
	}
	switch dst := m.Addrs[syscall.RTAX_DST].(type) {
	case *route.Inet4Addr:
		address = netip.AddrFrom4(dst.IP)
		if value, ok := netmask.(*route.Inet4Addr); ok {
			mask = net.IPMask(value.IP[:])
		}
	case *route.Inet6Addr:
		address = netip.AddrFrom16(dst.IP)
		if value, ok := netmask.(*route.Inet6Addr); ok {
			mask = net.IPMask(value.IP[:])
		}
	default:
		return netip.Prefix{}, nil // Non-IP routes cannot collide with TUN IP routes.
	}
	if m.Flags&syscall.RTF_HOST != 0 {
		return netip.PrefixFrom(address, address.BitLen()), nil
	}
	if mask == nil {
		if netmask == nil && address.IsUnspecified() {
			return netip.PrefixFrom(address, 0), nil
		}
		return netip.Prefix{}, errors.New("invalid route netmask")
	}
	ones, bits := mask.Size()
	if bits != address.BitLen() {
		return netip.Prefix{}, errors.New("invalid route netmask")
	}
	return netip.PrefixFrom(address, ones).Masked(), nil
}
