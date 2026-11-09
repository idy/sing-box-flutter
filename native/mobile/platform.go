// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2022 nekohasekai <contact-sagernet@sekai.icu>
// Adapted from SagerNet/sing-box v1.14.4; see NOTICE for the pinned revision.

package libbox

import (
	"context"
	"net"
	"net/netip"
	"runtime"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/control"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"

	"golang.org/x/sys/unix"
)

var _ adapter.PlatformInterface = (*platformInterfaceWrapper)(nil)

type platformInterfaceWrapper struct {
	platformInterfaceStub
	iif                    PlatformInterface
	networkManager         adapter.NetworkManager
	myTunName              string
	myTunAddress           []netip.Addr
	defaultInterfaceAccess sync.Mutex
	defaultInterface       *control.Interface
	pathInterfaceIndex     int32
	isExpensive            bool
	isConstrained          bool
}

func (w *platformInterfaceWrapper) Initialize(networkManager adapter.NetworkManager) error {
	w.networkManager = networkManager
	return nil
}

func (w *platformInterfaceWrapper) UsePlatformAutoDetectInterfaceControl() bool {
	return w.iif.UsePlatformAutoDetectInterfaceControl()
}

func (w *platformInterfaceWrapper) AutoDetectInterfaceControl(fd int) error {
	return w.iif.AutoDetectInterfaceControl(int32(fd))
}

func (w *platformInterfaceWrapper) UsePlatformInterface() bool {
	return true
}

func (w *platformInterfaceWrapper) OpenInterface(options *tun.Options, platformOptions option.TunPlatformOptions) (tun.Tun, error) {
	if len(options.IncludeUID) > 0 || len(options.ExcludeUID) > 0 {
		return nil, E.New("platform: unsupported uid options")
	}
	if len(options.IncludeAndroidUser) > 0 {
		return nil, E.New("platform: unsupported android_user option")
	}
	routeRanges, err := options.BuildAutoRouteRanges(true)
	if err != nil {
		return nil, err
	}
	tunFd, err := w.iif.OpenTun(&tunOptions{options, routeRanges, platformOptions})
	if err != nil {
		return nil, err
	}
	if tunFd < 0 {
		return nil, E.New("invalid tunnel descriptor")
	}
	options.Name, err = getTunnelName(tunFd)
	if err != nil {
		return nil, E.Cause(err, "query tun name")
	}
	if options.InterfaceMonitor != nil {
		options.InterfaceMonitor.RegisterMyInterface(options.Name)
	}
	dupFd, err := dup(int(tunFd))
	if err != nil {
		return nil, E.Cause(err, "dup tun file descriptor")
	}
	options.FileDescriptor = dupFd
	w.defaultInterfaceAccess.Lock()
	w.myTunName = options.Name
	w.myTunAddress = myTunAddress(options)
	w.defaultInterfaceAccess.Unlock()
	w.iif.RegisterMyInterface(options.Name)
	device, err := tun.New(*options)
	if err != nil {
		_ = unix.Close(dupFd)
		options.FileDescriptor = 0
		return nil, err
	}
	return device, nil
}

func (w *platformInterfaceWrapper) ProcessPlatformOptions(options option.TunPlatformOptions) error {
	return nil
}

func myTunAddress(options *tun.Options) []netip.Addr {
	addresses := make([]netip.Addr, 0, len(options.Inet4Address)+len(options.Inet6Address))
	for _, prefix := range options.Inet4Address {
		addresses = append(addresses, prefix.Addr())
	}
	for _, prefix := range options.Inet6Address {
		addresses = append(addresses, prefix.Addr())
	}
	return addresses
}

func (w *platformInterfaceWrapper) MyInterfaceAddress() []netip.Addr {
	w.defaultInterfaceAccess.Lock()
	defer w.defaultInterfaceAccess.Unlock()
	return append([]netip.Addr(nil), w.myTunAddress...)
}

func (w *platformInterfaceWrapper) UsePlatformDefaultInterfaceMonitor() bool {
	return true
}

func (w *platformInterfaceWrapper) CreateDefaultInterfaceMonitor(logger logger.Logger) tun.DefaultInterfaceMonitor {
	return &platformDefaultInterfaceMonitor{
		platformInterfaceWrapper: w,
		logger:                   logger,
	}
}

func (w *platformInterfaceWrapper) UsePlatformNetworkInterfaces() bool {
	return true
}

func (w *platformInterfaceWrapper) NetworkInterfaces() ([]adapter.NetworkInterface, error) {
	interfaceIterator, err := w.iif.GetInterfaces()
	if err != nil {
		return nil, err
	}
	var interfaces []adapter.NetworkInterface
	for _, netInterface := range iteratorToArray[*NetworkInterface](interfaceIterator) {
		if netInterface == nil {
			continue
		}
		w.defaultInterfaceAccess.Lock()
		// (GOOS=windows) SA4006: this value of `isDefault` is never used
		// Why not used?
		//nolint:staticcheck
		isDefault := netInterface.Name != w.myTunName && w.pathInterfaceIndex > 0 && netInterface.Index == w.pathInterfaceIndex
		expensive, constrained := w.isExpensive, w.isConstrained
		w.defaultInterfaceAccess.Unlock()
		var addresses []netip.Prefix
		for _, address := range iteratorToArray[string](netInterface.Addresses) {
			prefix, err := netip.ParsePrefix(address)
			if err != nil {
				return nil, E.New("platform supplied an invalid interface address")
			}
			addresses = append(addresses, prefix)
		}
		interfaces = append(interfaces, adapter.NetworkInterface{
			Interface: control.Interface{
				Index:     int(netInterface.Index),
				MTU:       int(netInterface.MTU),
				Name:      netInterface.Name,
				Addresses: addresses,
				Flags:     linkFlags(uint32(netInterface.Flags)),
			},
			Type:       C.InterfaceType(netInterface.Type),
			DNSServers: iteratorToArray[string](netInterface.DNSServer),
			Gateways: common.Filter(common.Map(iteratorToArray[string](netInterface.Gateway), func(it string) netip.Addr {
				gateway, _ := netip.ParseAddr(it)
				return gateway.Unmap().WithZone("")
			}), netip.Addr.IsValid),
			Expensive:   netInterface.Metered || isDefault && expensive,
			Constrained: isDefault && constrained,
		})
	}
	interfaces = common.UniqBy(interfaces, func(it adapter.NetworkInterface) string {
		return it.Name
	})
	return interfaces, nil
}

func (w *platformInterfaceWrapper) UnderNetworkExtension() bool {
	return w.iif.UnderNetworkExtension()
}

func (w *platformInterfaceWrapper) NetworkExtensionIncludeAllNetworks() bool {
	return w.iif.IncludeAllNetworks()
}

func (w *platformInterfaceWrapper) ClearDNSCache() {
	w.iif.ClearDNSCache()
}

func (w *platformInterfaceWrapper) RequestPermissionForWIFIState() error {
	return nil
}

func (w *platformInterfaceWrapper) UsePlatformWIFIMonitor() bool {
	return true
}

func (w *platformInterfaceWrapper) ReadWIFIState(ctx context.Context) adapter.WIFIState {
	wifiState := w.iif.ReadWIFIState()
	if wifiState == nil {
		return adapter.WIFIState{}
	}
	return adapter.WIFIState(*wifiState)
}

func (w *platformInterfaceWrapper) UsePlatformConnectionOwnerFinder() bool {
	return true
}

func (w *platformInterfaceWrapper) FindConnectionOwner(request *adapter.FindConnectionOwnerRequest) (*adapter.ConnectionOwner, error) {
	result, err := w.iif.FindConnectionOwner(request.IpProtocol, request.SourceAddress, request.SourcePort, request.DestinationAddress, request.DestinationPort)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, E.New("platform returned no connection owner")
	}
	return &adapter.ConnectionOwner{
		UserId:              result.UserId,
		UserName:            result.UserName,
		ProcessPath:         result.ProcessPath,
		AndroidPackageNames: result.androidPackageNames,
	}, nil
}

func (w *platformInterfaceWrapper) DisableColors() bool {
	return runtime.GOOS != "android"
}

func (w *platformInterfaceWrapper) UsePlatformNotification() bool {
	return true
}

func (w *platformInterfaceWrapper) SendNotification(notification *adapter.Notification) error {
	return w.iif.SendNotification((*Notification)(notification))
}

func (w *platformInterfaceWrapper) CancelNotification(identifier string, typeID int32) error {
	return w.iif.CancelNotification(identifier, typeID)
}

func (w *platformInterfaceWrapper) UsePlatformNeighborResolver() bool {
	return true
}

func (w *platformInterfaceWrapper) StartNeighborMonitor(listener adapter.NeighborUpdateListener) error {
	return w.iif.StartNeighborMonitor(&neighborUpdateListenerWrapper{listener: listener})
}

func (w *platformInterfaceWrapper) CloseNeighborMonitor(listener adapter.NeighborUpdateListener) error {
	return w.iif.CloseNeighborMonitor(nil)
}

func (w *platformInterfaceWrapper) TailscaleHostname() string {
	return w.iif.TailscaleHostname()
}

type neighborUpdateListenerWrapper struct {
	listener adapter.NeighborUpdateListener
}

func (w *neighborUpdateListenerWrapper) UpdateNeighborTable(entries NeighborEntryIterator) {
	var result []adapter.NeighborEntry
	for entries != nil && entries.HasNext() {
		entry := entries.Next()
		if entry == nil {
			continue
		}
		address, err := netip.ParseAddr(entry.Address)
		if err != nil {
			continue
		}
		macAddress, err := net.ParseMAC(entry.MacAddress)
		if err != nil {
			continue
		}
		result = append(result, adapter.NeighborEntry{
			Address:    address,
			MACAddress: macAddress,
			Hostname:   entry.Hostname,
		})
	}
	w.listener.UpdateNeighborTable(result)
}
