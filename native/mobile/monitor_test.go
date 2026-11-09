// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"net"
	"syscall"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common/control"
)

type monitorTestPlatform struct {
	PlatformInterface
	interfaces []*NetworkInterface
	closes     int
}

func (p *monitorTestPlatform) GetInterfaces() (NetworkInterfaceIterator, error) {
	return newIterator(p.interfaces), nil
}
func (*monitorTestPlatform) UsePlatformAutoDetectInterfaceControl() bool { return false }
func (*monitorTestPlatform) UnderNetworkExtension() bool                 { return false }
func (*monitorTestPlatform) IncludeAllNetworks() bool                    { return false }
func (*monitorTestPlatform) StartDefaultInterfaceMonitor(InterfaceUpdateListener) error {
	return nil
}
func (p *monitorTestPlatform) CloseDefaultInterfaceMonitor(InterfaceUpdateListener) error {
	p.closes++
	return nil
}

type monitorTestNetworkManager struct {
	adapter.NetworkManager
	wrapper      *platformInterfaceWrapper
	monitor      *platformDefaultInterfaceMonitor
	finder       *control.DefaultInterfaceFinder
	interfaces   []adapter.NetworkInterface
	enumerations int
	afterUpdate  func()
}

func (m *monitorTestNetworkManager) InterfaceFinder() control.InterfaceFinder { return m.finder }
func (m *monitorTestNetworkManager) NetworkInterfaces() []adapter.NetworkInterface {
	return m.interfaces
}
func (m *monitorTestNetworkManager) UpdateInterfaces() error {
	m.enumerations++
	interfaces, err := m.wrapper.NetworkInterfaces()
	if err != nil {
		return err
	}
	m.interfaces = interfaces
	var controls []control.Interface
	for _, iif := range interfaces {
		controls = append(controls, iif.Interface)
	}
	m.finder.UpdateInterfaces(controls)
	if m.afterUpdate != nil {
		m.afterUpdate()
	}
	return nil
}
func (m *monitorTestNetworkManager) DefaultNetworkInterface() *adapter.NetworkInterface {
	current := m.monitor.DefaultInterface()
	if current != nil {
		for _, iif := range m.interfaces {
			if iif.Index == current.Index {
				return &iif
			}
		}
	}
	return nil
}

func newMonitorFixture() (*platformDefaultInterfaceMonitor, *monitorTestNetworkManager, *monitorTestPlatform) {
	platform := &monitorTestPlatform{interfaces: []*NetworkInterface{
		{Index: 101, Name: "wifi-test", MTU: 1500, Flags: syscall.IFF_UP},
		{Index: 102, Name: "cell-test", MTU: 1500, Flags: syscall.IFF_UP},
	}}
	wrapper := &platformInterfaceWrapper{iif: platform}
	manager := &monitorTestNetworkManager{wrapper: wrapper, finder: control.NewDefaultInterfaceFinder()}
	wrapper.networkManager = manager
	monitor := &platformDefaultInterfaceMonitor{platformInterfaceWrapper: wrapper, logger: log.NewNOPFactory().Logger()}
	manager.monitor = monitor
	return monitor, manager, platform
}

func requirePathMetadata(t *testing.T, manager *monitorTestNetworkManager, wantIndex int, expensive, constrained bool) {
	t.Helper()
	current := manager.DefaultNetworkInterface()
	if wantIndex < 0 {
		if current != nil {
			t.Fatal("no-path update retained a default interface")
		}
	} else if current == nil || current.Index != wantIndex || current.Expensive != expensive || current.Constrained != constrained {
		t.Fatalf("default metadata=%+v, want index=%d expensive=%v constrained=%v", current, wantIndex, expensive, constrained)
	}
	for _, iif := range manager.interfaces {
		if iif.Index != wantIndex && (iif.Expensive || iif.Constrained) {
			t.Fatalf("old or unrelated interface retained path flags: %+v", iif)
		}
	}
	metadata := &adapter.InboundContext{}
	if got := rule.NewNetworkIsConstrainedItem(manager).Match(metadata); got != constrained {
		t.Fatalf("constrained rule match=%v, want %v", got, constrained)
	}
	if got := rule.NewNetworkIsExpensiveItem(manager).Match(metadata); got != expensive {
		t.Fatalf("expensive rule match=%v, want %v", got, expensive)
	}
}

func TestMonitorUsesIncomingPathMetadataOnFirstUpdateAndHandover(t *testing.T) {
	monitor, manager, _ := newMonitorFixture()
	callbacks := 0
	monitor.RegisterCallback(func(iif *control.Interface, _ int) {
		callbacks++
		// Callbacks must remain outside the shared lifecycle mutex.
		if monitor.DefaultInterface() != iif {
			t.Fatal("callback ran before the default pointer was updated")
		}
		temporary := monitor.RegisterCallback(func(*control.Interface, int) {})
		monitor.UnregisterCallback(temporary)
	})
	monitor.UpdateDefaultInterface("wifi-test", 101, true, true)
	requirePathMetadata(t, manager, 101, true, true)
	if callbacks != 1 || manager.enumerations != 1 {
		t.Fatal("first path must enumerate and notify once")
	}
	monitor.UpdateDefaultInterface("wifi-test", 101, true, true)
	if callbacks != 1 || manager.enumerations != 2 {
		t.Fatal("identical path must enumerate once and deduplicate callbacks")
	}
	monitor.UpdateDefaultInterface("wifi-test", 101, false, false)
	requirePathMetadata(t, manager, 101, false, false)
	if callbacks != 1 {
		t.Fatal("metadata-only refresh changed callback deduplication")
	}
	monitor.UpdateDefaultInterface("cell-test", 102, true, true)
	requirePathMetadata(t, manager, 102, true, true)
	monitor.UpdateDefaultInterface("", -1, false, false)
	requirePathMetadata(t, manager, -1, false, false)
	monitor.UpdateDefaultInterface("cell-test", 102, true, true)
	requirePathMetadata(t, manager, 102, true, true)
	if callbacks != 4 || manager.enumerations != 6 {
		t.Fatalf("callbacks=%d enumerations=%d", callbacks, manager.enumerations)
	}
}

func TestMonitorCloseDuringEnumerationAndLateCallbacks(t *testing.T) {
	monitor, manager, platform := newMonitorFixture()
	callbacks := 0
	monitor.RegisterCallback(func(*control.Interface, int) { callbacks++ })
	manager.afterUpdate = func() {
		if err := monitor.Close(); err != nil {
			t.Fatal(err)
		}
	}
	monitor.UpdateDefaultInterface("wifi-test", 101, true, true)
	monitor.UpdateDefaultInterface("cell-test", 102, false, false)
	monitor.updateDNSServers()
	if err := monitor.Close(); err != nil {
		t.Fatal(err)
	}
	if callbacks != 0 || manager.enumerations != 1 || platform.closes != 1 || monitor.DefaultInterface() != nil {
		t.Fatal("closed monitor published or enumerated a late update")
	}
}

func TestNetworkInterfacesRequirePositiveIncomingIndexAndExcludeTun(t *testing.T) {
	monitor, _, platform := newMonitorFixture()
	monitor.myTunName = "tun-test"
	platform.interfaces = append(platform.interfaces, &NetworkInterface{Index: 103, Name: "tun-test", MTU: 1500, Flags: syscall.IFF_UP})
	for _, index := range []int32{0, -1, 103} {
		monitor.defaultInterfaceAccess.Lock()
		monitor.pathInterfaceIndex = index
		monitor.isExpensive, monitor.isConstrained = true, true
		monitor.defaultInterfaceAccess.Unlock()
		interfaces, err := monitor.NetworkInterfaces()
		if err != nil {
			t.Fatal(err)
		}
		for _, iif := range interfaces {
			if iif.Flags&net.FlagUp == 0 || iif.Expensive || iif.Constrained {
				t.Fatalf("non-default path inherited flags: %+v", iif)
			}
		}
	}
}
