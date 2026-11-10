// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2022 nekohasekai <contact-sagernet@sekai.icu>
// Adapted from SagerNet/sing-box v1.14.4; see NOTICE for the pinned revision.

package libbox

import (
	"net/netip"
	"slices"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing-tun/dnsinfo"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/control"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/x/list"
)

var (
	_ tun.DefaultInterfaceMonitor = (*platformDefaultInterfaceMonitor)(nil)
	_ InterfaceUpdateListener     = (*platformDefaultInterfaceMonitor)(nil)
)

type platformDefaultInterfaceMonitor struct {
	*platformInterfaceWrapper
	host              DefaultInterfaceMonitorHost
	registerInterface func(string)
	logger            logger.Logger
	updates           updateQueue
	callbacks         list.List[tun.DefaultInterfaceUpdateCallback]
	myInterfaces      []string
	defaultDNSServers []string
	dnsWatcher        *dnsinfo.Watcher
	closed            bool
}

func (m *platformDefaultInterfaceMonitor) monitorHost() DefaultInterfaceMonitorHost {
	if m.host != nil {
		return m.host
	}
	return m.iif
}

func (m *platformDefaultInterfaceMonitor) Start() error {
	if C.IsDarwin {
		dnsWatcher, err := dnsinfo.NewWatcher(m.updateDNSServers, m.logger)
		if err != nil {
			return E.Cause(err, "watch DNS configuration")
		}
		m.dnsWatcher = dnsWatcher
	}
	err := m.monitorHost().StartDefaultInterfaceMonitor(m)
	if err != nil {
		if m.dnsWatcher != nil {
			_ = m.dnsWatcher.Close()
			m.dnsWatcher = nil
		}
		_ = m.monitorHost().CloseDefaultInterfaceMonitor(m)
		return err
	}
	return nil
}

func (m *platformDefaultInterfaceMonitor) Close() error {
	m.defaultInterfaceAccess.Lock()
	if m.closed {
		m.defaultInterfaceAccess.Unlock()
		return nil
	}
	m.closed = true
	m.defaultInterfaceAccess.Unlock()
	err := m.monitorHost().CloseDefaultInterfaceMonitor(m)
	if m.dnsWatcher != nil {
		err = E.Errors(err, m.dnsWatcher.Close())
		m.dnsWatcher = nil
	}
	return err
}

func (m *platformDefaultInterfaceMonitor) DefaultInterface() *control.Interface {
	m.defaultInterfaceAccess.Lock()
	defer m.defaultInterfaceAccess.Unlock()
	return m.defaultInterface
}

func (m *platformDefaultInterfaceMonitor) OverrideAndroidVPN() bool {
	return false
}

func (m *platformDefaultInterfaceMonitor) AndroidVPNEnabled() bool {
	return false
}

func (m *platformDefaultInterfaceMonitor) RegisterCallback(callback tun.DefaultInterfaceUpdateCallback) *list.Element[tun.DefaultInterfaceUpdateCallback] {
	m.defaultInterfaceAccess.Lock()
	defer m.defaultInterfaceAccess.Unlock()
	return m.callbacks.PushBack(callback)
}

func (m *platformDefaultInterfaceMonitor) UnregisterCallback(element *list.Element[tun.DefaultInterfaceUpdateCallback]) {
	m.defaultInterfaceAccess.Lock()
	defer m.defaultInterfaceAccess.Unlock()
	m.callbacks.Remove(element)
}

// Network path text is intentionally not logged; it may identify a user network.
func (m *platformDefaultInterfaceMonitor) UpdateNetworkPath(networkPath string) {}

func (m *platformDefaultInterfaceMonitor) UpdateDefaultInterface(interfaceName string, interfaceIndex32 int32, isExpensive bool, isConstrained bool) {
	if sFixAndroidStack.Load() {
		done := make(chan struct{})
		go func() {
			m.updateDefaultInterface(interfaceName, interfaceIndex32, isExpensive, isConstrained)
			close(done)
		}()
		<-done
	} else {
		m.updateDefaultInterface(interfaceName, interfaceIndex32, isExpensive, isConstrained)
	}
}

func (m *platformDefaultInterfaceMonitor) updateDefaultInterface(interfaceName string, interfaceIndex32 int32, isExpensive bool, isConstrained bool) {
	// Hosts report paths from several threads (Android: the listener seed on
	// the start thread and the connectivity handler). Hold the queue across
	// enumeration and callbacks so an older update cannot publish after a newer
	// one; readers still use defaultInterfaceAccess. Callbacks must not report a
	// path or DNS change synchronously. Close never enters the queue.
	leave := m.updates.enter()
	defer leave()
	m.defaultInterfaceAccess.Lock()
	if m.closed || slices.Contains(m.myInterfaces, interfaceName) {
		m.defaultInterfaceAccess.Unlock()
		return
	}
	m.isExpensive = isExpensive
	m.isConstrained = isConstrained
	m.pathInterfaceIndex = interfaceIndex32
	m.defaultInterfaceAccess.Unlock()
	err := m.networkManager.UpdateInterfaces()
	if err != nil {
		m.logger.Error(E.Cause(err, "update interfaces"))
	}
	m.defaultInterfaceAccess.Lock()
	if m.closed {
		m.defaultInterfaceAccess.Unlock()
		return
	}
	if interfaceIndex32 == -1 {
		m.defaultInterface = nil
		m.defaultDNSServers = nil
		callbacks := m.callbacks.Array()
		m.defaultInterfaceAccess.Unlock()
		for _, callback := range callbacks {
			callback(nil, 0)
		}
		return
	}
	oldInterface := m.defaultInterface
	newInterface, err := m.networkManager.InterfaceFinder().ByIndex(int(interfaceIndex32))
	if err != nil {
		m.defaultInterfaceAccess.Unlock()
		m.logger.Error(E.Cause(err, "find updated interface: ", interfaceName))
		return
	}
	// A host may have selected this path before RegisterMyInterface learned its
	// dynamically assigned name. Test ownership under the assignment mutex, so
	// neither an in-flight enumeration nor a queued stale report can restore it.
	if slices.Contains(m.myInterfaces, newInterface.Name) {
		m.defaultInterfaceAccess.Unlock()
		return
	}
	oldDNSServers := m.defaultDNSServers
	m.defaultInterface = newInterface
	m.defaultDNSServers = m.readDNSServers(newInterface.Index)
	if oldInterface != nil && oldInterface.Name == m.defaultInterface.Name && oldInterface.Index == m.defaultInterface.Index && slices.Equal(oldDNSServers, m.defaultDNSServers) {
		m.defaultInterfaceAccess.Unlock()
		return
	}
	callbacks := m.callbacks.Array()
	m.defaultInterfaceAccess.Unlock()
	for _, callback := range callbacks {
		callback(newInterface, 0)
	}
}

// DNS watcher reports publish the same callbacks, so they share the queue.
func (m *platformDefaultInterfaceMonitor) updateDNSServers() {
	leave := m.updates.enter()
	defer leave()
	m.defaultInterfaceAccess.Lock()
	defaultInterface := m.defaultInterface
	if m.closed || defaultInterface == nil {
		m.defaultInterfaceAccess.Unlock()
		return
	}
	dnsServers := m.readDNSServers(defaultInterface.Index)
	if slices.Equal(m.defaultDNSServers, dnsServers) {
		m.defaultInterfaceAccess.Unlock()
		return
	}
	m.defaultDNSServers = dnsServers
	callbacks := m.callbacks.Array()
	m.defaultInterfaceAccess.Unlock()
	for _, callback := range callbacks {
		callback(defaultInterface, 0)
	}
}

// updateQueue admits path publications one at a time in arrival order. A
// sync.Mutex would let a releasing host thread report again and overtake an
// earlier waiter, so a stale path could still be published last.
type updateQueue struct {
	access sync.Mutex
	tail   chan struct{}
}

func (q *updateQueue) enter() (leave func()) {
	done := make(chan struct{})
	q.access.Lock()
	previous := q.tail
	q.tail = done
	q.access.Unlock()
	if previous != nil {
		<-previous
	}
	return func() { close(done) }
}

func (m *platformDefaultInterfaceMonitor) readDNSServers(interfaceIndex int) []string {
	if C.IsDarwin {
		dnsConfiguration := dnsinfo.Copy()
		if dnsConfiguration == nil {
			return nil
		}
		return common.Map(dnsConfiguration.Select(interfaceIndex).Servers, func(it netip.AddrPort) string {
			return it.Addr().String()
		})
	}
	return common.Find(m.networkManager.NetworkInterfaces(), func(it adapter.NetworkInterface) bool {
		return it.Index == interfaceIndex
	}).DNSServers
}

func (m *platformDefaultInterfaceMonitor) RegisterMyInterface(interfaceName string) {
	m.defaultInterfaceAccess.Lock()
	if m.closed || slices.Contains(m.myInterfaces, interfaceName) {
		m.defaultInterfaceAccess.Unlock()
		return
	}
	m.myInterfaces = append(m.myInterfaces, interfaceName)
	// Registration may follow publication of the new interface. Withdraw that
	// pointer atomically with ownership; Start/route callers must never wait for
	// the host's asynchronous refresh before seeing that it is not an uplink.
	if m.defaultInterface != nil && m.defaultInterface.Name == interfaceName {
		m.defaultInterface = nil
		m.defaultDNSServers = nil
	}
	m.defaultInterfaceAccess.Unlock()
	// Hosts may enqueue a refreshed path. Never invoke them while holding the
	// interface mutex: their listener can re-enter the monitor asynchronously.
	if m.registerInterface != nil {
		m.registerInterface(interfaceName)
	}
}

func (m *platformDefaultInterfaceMonitor) MyInterfaces() []string {
	m.defaultInterfaceAccess.Lock()
	defer m.defaultInterfaceAccess.Unlock()
	return append([]string(nil), m.myInterfaces...)
}
