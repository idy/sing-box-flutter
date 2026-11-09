// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2022 nekohasekai <contact-sagernet@sekai.icu>
// Adapted from SagerNet/sing-box v1.14.4; see NOTICE for the pinned revision.

package libbox

import (
	"context"
	"net/netip"
	"os"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/x/list"
)

type platformInterfaceStub struct{}

func (s *platformInterfaceStub) Initialize(networkManager adapter.NetworkManager) error {
	return nil
}

func (s *platformInterfaceStub) UsePlatformAutoDetectInterfaceControl() bool {
	return true
}

func (s *platformInterfaceStub) AutoDetectInterfaceControl(fd int) error {
	return nil
}

func (s *platformInterfaceStub) UsePlatformInterface() bool {
	return false
}

func (s *platformInterfaceStub) OpenInterface(options *tun.Options, platformOptions option.TunPlatformOptions) (tun.Tun, error) {
	return nil, os.ErrInvalid
}

func (s *platformInterfaceStub) ProcessPlatformOptions(options option.TunPlatformOptions) error {
	return nil
}

func (s *platformInterfaceStub) UsePlatformDefaultInterfaceMonitor() bool {
	return true
}

func (s *platformInterfaceStub) CreateDefaultInterfaceMonitor(logger logger.Logger) tun.DefaultInterfaceMonitor {
	return &interfaceMonitorStub{}
}

func (s *platformInterfaceStub) UsePlatformNetworkInterfaces() bool {
	return false
}

func (s *platformInterfaceStub) NetworkInterfaces() ([]adapter.NetworkInterface, error) {
	return nil, os.ErrInvalid
}

func (s *platformInterfaceStub) UnderNetworkExtension() bool {
	return false
}

func (s *platformInterfaceStub) NetworkExtensionIncludeAllNetworks() bool {
	return false
}

func (s *platformInterfaceStub) ClearDNSCache() {
}

func (s *platformInterfaceStub) RequestPermissionForWIFIState() error {
	return nil
}

func (s *platformInterfaceStub) UsePlatformWIFIMonitor() bool {
	return false
}

func (s *platformInterfaceStub) ReadWIFIState(ctx context.Context) adapter.WIFIState {
	return adapter.WIFIState{}
}

func (s *platformInterfaceStub) UsePlatformConnectionOwnerFinder() bool {
	return false
}

func (s *platformInterfaceStub) FindConnectionOwner(request *adapter.FindConnectionOwnerRequest) (*adapter.ConnectionOwner, error) {
	return nil, os.ErrInvalid
}

func (s *platformInterfaceStub) UsePlatformNotification() bool {
	return false
}

func (s *platformInterfaceStub) SendNotification(notification *adapter.Notification) error {
	return os.ErrInvalid
}

func (s *platformInterfaceStub) CancelNotification(identifier string, typeID int32) error {
	return nil
}

func (s *platformInterfaceStub) MyInterfaceAddress() []netip.Addr {
	return nil
}

func (s *platformInterfaceStub) UsePlatformNeighborResolver() bool {
	return false
}

func (s *platformInterfaceStub) StartNeighborMonitor(listener adapter.NeighborUpdateListener) error {
	return os.ErrInvalid
}

func (s *platformInterfaceStub) CloseNeighborMonitor(listener adapter.NeighborUpdateListener) error {
	return nil
}

func (s *platformInterfaceStub) UsePlatformShell() bool {
	return false
}

func (s *platformInterfaceStub) CheckPlatformShell() error {
	return os.ErrInvalid
}

func (s *platformInterfaceStub) OpenShellSession(user *adapter.PlatformUser, command string, env []string, term string, rows int32, cols int32) (adapter.ShellSession, error) {
	return nil, os.ErrInvalid
}

func (s *platformInterfaceStub) LookupSFTPServer() (string, error) {
	return "", os.ErrInvalid
}

func (s *platformInterfaceStub) ReadSystemSSHHostKey() ([]byte, error) {
	return nil, os.ErrInvalid
}

func (s *platformInterfaceStub) TailscaleHostname() string {
	return ""
}

func (s *platformInterfaceStub) UsePlatformBridge() bool {
	return false
}

func (s *platformInterfaceStub) CreateBridge(options adapter.BridgeOptions) (adapter.BridgeSession, error) {
	return nil, os.ErrInvalid
}

func (s *platformInterfaceStub) LookupUser(username string) (*adapter.PlatformUser, error) {
	return nil, os.ErrInvalid
}

func (s *platformInterfaceStub) UsePlatformLocalDNSTransport() bool {
	return false
}

func (s *platformInterfaceStub) LocalDNSTransport() dns.TransportConstructorFunc[option.LocalDNSServerOptions] {
	return nil
}

type interfaceMonitorStub struct{}

func (s *interfaceMonitorStub) Start() error {
	return os.ErrInvalid
}

func (s *interfaceMonitorStub) Close() error {
	return nil
}

func (s *interfaceMonitorStub) DefaultInterface() *control.Interface {
	return nil
}

func (s *interfaceMonitorStub) OverrideAndroidVPN() bool {
	return false
}

func (s *interfaceMonitorStub) AndroidVPNEnabled() bool {
	return false
}

func (s *interfaceMonitorStub) RegisterCallback(callback tun.DefaultInterfaceUpdateCallback) *list.Element[tun.DefaultInterfaceUpdateCallback] {
	return nil
}

func (s *interfaceMonitorStub) UnregisterCallback(element *list.Element[tun.DefaultInterfaceUpdateCallback]) {
}

func (s *interfaceMonitorStub) RegisterMyInterface(interfaceName string) {
}

func (s *interfaceMonitorStub) MyInterfaces() []string {
	return nil
}
