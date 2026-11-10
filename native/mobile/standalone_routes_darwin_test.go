// SPDX-License-Identifier: GPL-3.0-or-later
//go:build darwin

package libbox

import (
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/net/route"
)

func TestStandaloneDarwinRoutePrefixes(t *testing.T) {
	v4 := func(ip [4]byte) route.Addr { return &route.Inet4Addr{IP: ip} }
	v6 := func(text string) route.Addr { return &route.Inet6Addr{IP: netip.MustParseAddr(text).As16()} }
	for _, tc := range []struct {
		name      string
		dst, mask route.Addr
		flags     int
		want      string
		fails     bool
	}{
		{"IPv4 netmask", v4([4]byte{198, 18, 20, 99}), v4([4]byte{255, 255, 255, 0}), 0, "198.18.20.0/24", false},
		{"IPv4 host", v4([4]byte{198, 18, 20, 1}), nil, syscall.RTF_HOST, "198.18.20.1/32", false},
		{"IPv4 default empty mask", v4([4]byte{}), nil, 0, "0.0.0.0/0", false},
		{"IPv4 default zero mask", v4([4]byte{}), v4([4]byte{}), 0, "0.0.0.0/0", false},
		{"IPv6 network", v6("2001:db8::1"), v6("ffff:ffff:ffff:ffff::"), 0, "2001:db8::/64", false},
		{"IPv6 host", v6("2001:db8::1"), nil, syscall.RTF_HOST, "2001:db8::1/128", false},
		{"IPv6 default", v6("::"), v6("::"), 0, "::/0", false},
		{"missing nondefault mask", v4([4]byte{198, 18, 20, 0}), nil, 0, "", true},
		{"invalid mask", v4([4]byte{198, 18, 20, 0}), v4([4]byte{255, 0, 255, 0}), 0, "", true},
		{"wrong mask family", v4([4]byte{198, 18, 20, 0}), v6("::"), 0, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &route.RouteMessage{Flags: tc.flags, Addrs: make([]route.Addr, syscall.RTAX_NETMASK+1)}
			m.Addrs[syscall.RTAX_DST], m.Addrs[syscall.RTAX_NETMASK] = tc.dst, tc.mask
			got, err := standaloneRoutePrefix(m)
			if (err != nil) != tc.fails || (err == nil && got.String() != tc.want) {
				t.Fatalf("route prefix=%v err=%v, want %s failure=%v", got, err, tc.want, tc.fails)
			}
		})
	}
}

func TestStandaloneDarwinReadsRoutesWithoutChangingNetworking(t *testing.T) {
	prefixes, err := readStandaloneRoutes()
	if err != nil {
		t.Fatalf("read-only route table inspection failed: %v", err)
	}
	if len(prefixes) == 0 {
		t.Fatal("route table inspection returned no IP routes")
	}
}

func TestStandaloneDarwinAddOnlyRouteOwnership(t *testing.T) {
	for _, cidr := range []string{"198.18.20.1/32", "fd00::/126"} {
		destination := netip.MustParsePrefix(cidr)
		gateway := netip.MustParseAddr("198.18.10.1")
		if destination.Addr().Is6() {
			gateway = netip.MustParseAddr("fd00::1")
		}
		ours := standaloneRoute{destination: destination, interfaceRoute: true, interfaceIndex: 42}
		foreign := ours
		foreign.interfaceIndex = 43
		gatewayRoute := ours
		gatewayRoute.interfaceRoute = false
		scoped := ours
		scoped.scoped = true
		for _, tc := range []struct {
			name    string
			table   []standaloneRoute
			readErr error
			accept  bool
		}{
			{"own-connected", []standaloneRoute{ours}, nil, true},
			{"other-interface", []standaloneRoute{foreign}, nil, false},
			{"own-gateway", []standaloneRoute{gatewayRoute}, nil, false},
			{"own-scoped-only", []standaloneRoute{scoped}, nil, false},
			{"scoped-own-and-unscoped-foreign", []standaloneRoute{scoped, foreign}, nil, false},
			{"own-and-ambiguous-foreign", []standaloneRoute{ours, foreign}, nil, false},
			{"missing", nil, nil, false},
			{"inspection-fails", nil, errors.New("private-route-details"), false},
		} {
			t.Run(cidr+"/"+tc.name, func(t *testing.T) {
				before := append([]standaloneRoute(nil), tc.table...)
				writes, reads := 0, 0
				err := installStandaloneRoute(42, destination, gateway, func(message *route.RouteMessage) error {
					writes++
					if message.Type != syscall.RTM_ADD || message.Index != 42 || message.Flags&syscall.RTF_IFSCOPE != 0 || message.Flags&syscall.RTF_GATEWAY == 0 {
						t.Fatalf("unexpected route mutation: %+v", message)
					}
					if link, ok := message.Addrs[syscall.RTAX_IFP].(*route.LinkAddr); !ok || link.Index != 42 {
						t.Fatal("route is not attached to the owned utun")
					}
					prefix, err := standaloneRoutePrefix(message)
					if err != nil || prefix != destination {
						t.Fatalf("route encoding changed destination: %v, %v", prefix, err)
					}
					return syscall.EEXIST
				}, func() ([]standaloneRoute, error) {
					reads++
					return tc.table, tc.readErr
				})
				if (err == nil) != tc.accept || (err != nil && !errors.Is(err, syscall.EEXIST)) || writes != 1 || reads != 1 || !reflect.DeepEqual(tc.table, before) {
					t.Fatalf("EEXIST replaced a route or lost ownership checks: err=%v writes=%d reads=%d table=%v", err, writes, reads, tc.table)
				}
			})
		}
	}
}

func TestStandaloneDarwinRouteRequestsUseDistinctSequences(t *testing.T) {
	const count = 32
	sequences := make(chan int, count)
	var workers sync.WaitGroup
	for range count {
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := installStandaloneRoute(42, netip.MustParsePrefix("198.18.20.1/32"), netip.MustParseAddr("198.18.10.1"), func(message *route.RouteMessage) error {
				sequences <- message.Seq
				return nil
			}, nil)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	close(sequences)
	seen := make(map[int]bool)
	for sequence := range sequences {
		if seen[sequence] {
			t.Fatal("concurrent route requests share an acknowledgement sequence")
		}
		seen[sequence] = true
	}
	if len(seen) != count {
		t.Fatal("route requests were lost")
	}
}

func TestStandaloneDarwinRouteAcknowledgements(t *testing.T) {
	request := &route.RouteMessage{Type: syscall.RTM_ADD, ID: 123, Seq: 1}
	for _, routeErr := range []error{nil, syscall.EEXIST, syscall.ENETUNREACH} {
		messages := []route.Message{
			&route.RouteMessage{Type: syscall.RTM_ADD, ID: 999, Seq: 1},
			&route.RouteMessage{Type: syscall.RTM_ADD, ID: 123, Seq: 2},
			&route.RouteMessage{Type: syscall.RTM_DELETE, ID: 123, Seq: 1},
		}
		if matched, err := standaloneRouteReply(request, messages); matched || err != nil {
			t.Fatal("accepted an unrelated route acknowledgement")
		}
		messages = append(messages, &route.RouteMessage{Type: syscall.RTM_ADD, ID: 123, Seq: 1, Err: routeErr})
		if matched, err := standaloneRouteReply(request, messages); !matched || !errors.Is(err, routeErr) {
			t.Fatalf("kernel route failure was lost: %v", err)
		}
	}
	// Refuse a destructive operation before even opening a real route socket.
	if err := writeStandaloneRoute(&route.RouteMessage{Type: syscall.RTM_DELETE}); err == nil {
		t.Fatal("route writer accepted DELETE")
	}
}
