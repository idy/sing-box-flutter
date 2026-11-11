// SPDX-License-Identifier: GPL-3.0-or-later
// Platform integration adapted from SagerNet/sing-box-for-apple:
// Library/Network/ExtensionPlatformInterface.swift (GPL-3.0-or-later).
// Copyright (C) 2022 nekohasekai <contact-sagernet@sekai.icu>.
// See ios/NOTICE for pinned reference sources. Bindings target sing-box v1.14.4.
import Darwin
import Foundation
import Libbox
import Network
import NetworkExtension

final class ApplePlatformInterface: NSObject, LibboxPlatformInterfaceProtocol, LibboxCommandServerHandlerProtocol {
    private weak var provider: SingBoxPacketTunnelProvider?
    private let lock = NSLock()
    private let monitorQueue = DispatchQueue(label: "dev.idy.sing_box_flutter.network-monitor")
    private let settingsQueue = DispatchQueue(label: "dev.idy.sing_box_flutter.network-settings")
    private var monitor: NWPathMonitor?
    private var currentPath: Network.NWPath?
    private var networkSettings: NEPacketTunnelNetworkSettings?

    init(provider: SingBoxPacketTunnelProvider) { self.provider = provider }

    var hasTunnel: Bool {
        lock.lock(); defer { lock.unlock() }
        return networkSettings != nil
    }

    func openTun(_ options: LibboxTunOptionsProtocol?, ret0_: UnsafeMutablePointer<Int32>?) throws {
        guard let options, let ret0_, let provider else { throw TunnelFailure.invalidArguments }
        guard !hasTunnel else { throw TunnelFailure(code: "multiple_tun", message: "Only one TUN inbound is supported by the Network Extension backend.") }
        if !strings(options.getIncludePackage()).isEmpty || !strings(options.getExcludePackage()).isEmpty || options.getStrictRoute() {
            throw TunnelFailure(code: "unsupported_tun_option", message: "Per-app routing and strict_route are not supported by the Network Extension backend.")
        }
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        settings.mtu = NSNumber(value: options.getMTU())

        let addresses4 = prefixes(options.getInet4Address())
        if !addresses4.isEmpty {
            let ipv4 = NEIPv4Settings(addresses: addresses4.map { $0.address() }, subnetMasks: addresses4.map { $0.mask() })
            // libbox builds these ranges from route_address, auto_route and exclusions.
            ipv4.includedRoutes = prefixes(options.getInet4RouteRange()).map {
                NEIPv4Route(destinationAddress: $0.address(), subnetMask: $0.mask())
            }
            ipv4.excludedRoutes = prefixes(options.getInet4RouteExcludeAddress()).map {
                NEIPv4Route(destinationAddress: $0.address(), subnetMask: $0.mask())
            }
            settings.ipv4Settings = ipv4
        }
        let addresses6 = prefixes(options.getInet6Address())
        if !addresses6.isEmpty {
            let ipv6 = NEIPv6Settings(addresses: addresses6.map { $0.address() }, networkPrefixLengths: addresses6.map { NSNumber(value: $0.prefix()) })
            ipv6.includedRoutes = prefixes(options.getInet6RouteRange()).map {
                NEIPv6Route(destinationAddress: $0.address(), networkPrefixLength: NSNumber(value: $0.prefix()))
            }
            ipv6.excludedRoutes = prefixes(options.getInet6RouteExcludeAddress()).map {
                NEIPv6Route(destinationAddress: $0.address(), networkPrefixLength: NSNumber(value: $0.prefix()))
            }
            settings.ipv6Settings = ipv6
        }
        if options.getAutoRoute(), options.getDNSMode()?.value != LibboxDNSModeDisabled {
            let servers = strings(try options.getDNSServerAddress())
            if !servers.isEmpty {
                let dns = NEDNSSettings(servers: servers)
                dns.matchDomains = [""]
                dns.matchDomainsNoSearch = true
                settings.dnsSettings = dns
            }
        }
        if options.isHTTPProxyEnabled() {
            let proxy = NEProxySettings()
            let server = NEProxyServer(address: options.getHTTPProxyServer(), port: Int(options.getHTTPProxyServerPort()))
            proxy.httpServer = server
            proxy.httpsServer = server
            proxy.httpEnabled = true
            proxy.httpsEnabled = true
            proxy.exceptionList = strings(options.getHTTPProxyBypassDomain())
            let matchDomains = strings(options.getHTTPProxyMatchDomain())
            if !matchDomains.isEmpty { proxy.matchDomains = matchDomains }
            settings.proxySettings = proxy
        }
        try apply(settings, to: provider)
        // Use libbox's official utun descriptor lookup, also used by sing-box-for-apple.
        // libbox duplicates the descriptor in platformInterfaceWrapper.OpenInterface;
        // NEPacketTunnelFlow continues to own the original, which must not be closed here.
        let descriptor = LibboxGetTunnelFileDescriptor()
        guard descriptor >= 0 else {
            throw TunnelFailure(code: "tun_descriptor_unavailable", message: "The system did not expose the packet tunnel's file descriptor.")
        }
        lock.lock()
        networkSettings = settings
        lock.unlock()
        ret0_.pointee = descriptor
    }

    private func apply(_ settings: NEPacketTunnelNetworkSettings?, to provider: SingBoxPacketTunnelProvider) throws {
        let complete = DispatchSemaphore(value: 0)
        let reply = SettingsReply()
        provider.setTunnelNetworkSettings(settings) { error in
            reply.set(error)
            complete.signal()
        }
        guard complete.wait(timeout: .now() + 15) == .success else {
            throw TunnelFailure(code: "network_settings_timeout", message: "The system timed out while applying tunnel network settings.")
        }
        if reply.failed {
            throw TunnelFailure(code: "network_settings_failed", message: "The system rejected the tunnel network settings.")
        }
    }

    func startDefaultInterfaceMonitor(_ listener: LibboxInterfaceUpdateListenerProtocol?) throws {
        guard let listener else { throw TunnelFailure.invalidArguments }
        let monitor = NWPathMonitor()
        lock.lock()
        guard self.monitor == nil else { lock.unlock(); throw TunnelFailure.busy }
        self.monitor = monitor
        lock.unlock()
        let firstPath = DispatchSemaphore(value: 0)
        monitor.pathUpdateHandler = { [weak self, weak monitor] path in
            guard let self, let monitor else { return }
            self.lock.lock()
            guard self.monitor === monitor else { self.lock.unlock(); return }
            self.currentPath = path
            self.lock.unlock()
            listener.updateNetworkPath(self.describe(path))
            // availableInterfaces is ordered by system preference. Follow the
            // official adapter even while a cellular path requires connection.
            let interface = path.availableInterfaces.first
            if path.status == .satisfied, let interface {
                listener.updateDefaultInterface(interface.name, interfaceIndex: Int32(interface.index), isExpensive: path.isExpensive, isConstrained: path.isConstrained)
            } else {
                listener.updateDefaultInterface("", interfaceIndex: -1, isExpensive: false, isConstrained: false)
            }
            firstPath.signal()
        }
        monitor.start(queue: monitorQueue)
        guard firstPath.wait(timeout: .now() + 5) == .success else {
            resetMonitor()
            throw TunnelFailure(code: "network_monitor_timeout", message: "The system did not provide an initial network path.")
        }
    }

    func closeDefaultInterfaceMonitor(_ listener: LibboxInterfaceUpdateListenerProtocol?) throws { resetMonitor() }

    private func resetMonitor() {
        lock.lock()
        let old = monitor
        monitor = nil
        currentPath = nil
        lock.unlock()
        old?.pathUpdateHandler = nil
        old?.cancel()
        // An in-flight callback retains its Go listener until it returns. Do not wait
        // here: Go can close the monitor while holding service/network locks.
    }

    func getInterfaces() throws -> LibboxNetworkInterfaceIteratorProtocol {
        lock.lock()
        let path = currentPath
        lock.unlock()
        guard let path else { throw TunnelFailure(code: "network_monitor_unavailable", message: "The network path monitor is not running.") }
        guard path.status == .satisfied else { return NetworkInterfaceIterator([]) }
        var addressList: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&addressList) == 0 else {
            throw TunnelFailure(code: "interfaces_unavailable", message: "System network interfaces could not be enumerated.")
        }
        defer { freeifaddrs(addressList) }
        var addresses: [String: [String]] = [:]
        var flags: [String: UInt32] = [:]
        var mtus: [String: Int32] = [:]
        var cursor = addressList
        while let entry = cursor?.pointee {
            let name = String(cString: entry.ifa_name)
            flags[name] = entry.ifa_flags
            if entry.ifa_addr?.pointee.sa_family == UInt8(AF_LINK), let data = entry.ifa_data {
                mtus[name] = Int32(data.assumingMemoryBound(to: if_data.self).pointee.ifi_mtu)
            }
            if let address = entry.ifa_addr, let mask = entry.ifa_netmask,
               let prefix = interfaceAddress(address, mask: mask) {
                addresses[name, default: []].append(prefix)
            }
            cursor = entry.ifa_next
        }
        let interfaces = path.availableInterfaces.map { interface -> LibboxNetworkInterface in
            let value = LibboxNetworkInterface()
            value.name = interface.name
            value.index = Int32(interface.index)
            value.mtu = mtus[interface.name] ?? 1500
            value.flags = Int32(bitPattern: flags[interface.name] ?? 0)
            value.addresses = StringIterator(addresses[interface.name] ?? [])
            value.dnsServer = StringIterator([])
            value.gateway = StringIterator([])
            value.metered = path.isExpensive && path.usesInterfaceType(interface.type)
            switch interface.type {
            case .wifi: value.type = LibboxInterfaceTypeWIFI
            case .cellular: value.type = LibboxInterfaceTypeCellular
            case .wiredEthernet: value.type = LibboxInterfaceTypeEthernet
            default: value.type = LibboxInterfaceTypeOther
            }
            return value
        }
        return NetworkInterfaceIterator(interfaces)
    }

    private func interfaceAddress(_ address: UnsafeMutablePointer<sockaddr>, mask: UnsafeMutablePointer<sockaddr>) -> String? {
        let family = Int32(address.pointee.sa_family)
        guard family == AF_INET || family == AF_INET6 else { return nil }
        var host = [CChar](repeating: 0, count: Int(NI_MAXHOST))
        guard getnameinfo(address, socklen_t(address.pointee.sa_len), &host, socklen_t(host.count), nil, 0, NI_NUMERICHOST) == 0 else { return nil }
        let bits: Int
        if family == AF_INET {
            bits = mask.withMemoryRebound(to: sockaddr_in.self, capacity: 1) {
                $0.pointee.sin_addr.s_addr.nonzeroBitCount
            }
        } else {
            bits = mask.withMemoryRebound(to: sockaddr_in6.self, capacity: 1) {
                withUnsafeBytes(of: $0.pointee.sin6_addr) { $0.reduce(0) { $0 + $1.nonzeroBitCount } }
            }
        }
        // A zone identifier is not accepted in the netip.Prefix strings consumed by libbox.
        return String(cString: host).components(separatedBy: "%")[0] + "/" + String(bits)
    }

    private func describe(_ path: Network.NWPath) -> String {
        let state = path.status == .satisfied ? "satisfied" : "unavailable"
        return state + " " + path.availableInterfaces.map { "\($0.name)#\($0.index)" }.joined(separator: ",")
            + " expensive=\(path.isExpensive) constrained=\(path.isConstrained)"
    }

    func clearDNSCache() {
        // DNS cache invalidation has no throwing API in libbox. Fail the tunnel if restoring
        // settings fails, instead of silently continuing without its DNS/routing policy.
        settingsQueue.async { [weak self] in
            guard let self, let provider = self.provider else { return }
            self.lock.lock()
            let settings = self.networkSettings
            self.lock.unlock()
            guard let settings else { return }
            provider.reasserting = true
            defer { provider.reasserting = false }
            do {
                try self.apply(nil, to: provider)
                try self.apply(settings, to: provider)
            } catch { provider.failFromPlatform() }
        }
    }

    func reset() {
        resetMonitor()
        settingsQueue.sync {}
        lock.lock()
        networkSettings = nil
        lock.unlock()
    }

    func usePlatformAutoDetectControl() -> Bool { false }
    func autoDetectControl(_ fd: Int32) throws { throw TunnelFailure.unsupported }
    func underNetworkExtension() -> Bool { true }
    func includeAllNetworks() -> Bool { provider?.protocolConfiguration.includeAllNetworks ?? false }
    func localDNSTransport() -> LibboxLocalDNSTransportProtocol? { nil }
    func readWIFIState() -> LibboxWIFIState? { nil }
    func tailscaleHostname() -> String {
        #if os(macOS)
        return "sing-box-macos"
        #else
        return "sing-box-ios"
        #endif
    }
    func registerMyInterface(_ name: String?) {} // No privileged helper; NE owns the utun.

    func findConnectionOwner(_ ipProtocol: Int32, sourceAddress: String?, sourcePort: Int32, destinationAddress: String?, destinationPort: Int32) throws -> LibboxConnectionOwner { throw TunnelFailure.unsupported }
    func send(_ notification: LibboxNotification?) throws { throw TunnelFailure.unsupported }
    func cancelNotification(_ identifier: String?, typeID: Int32) throws { throw TunnelFailure.unsupported }
    func startNeighborMonitor(_ listener: LibboxNeighborUpdateListenerProtocol?) throws { throw TunnelFailure.unsupported }
    func closeNeighborMonitor(_ listener: LibboxNeighborUpdateListenerProtocol?) throws {} // Idempotent cleanup.
    func usePlatformShell() -> Bool { false }
    func checkPlatformShell() throws { throw TunnelFailure.unsupported }
    func openShellSession(_ user: LibboxPlatformUser?, command: String?, environ: LibboxStringIteratorProtocol?, term: String?, rows: Int32, cols: Int32) throws -> LibboxShellSessionProtocol { throw TunnelFailure.unsupported }
    func lookupUser(_ username: String?) throws -> LibboxPlatformUser { throw TunnelFailure.unsupported }
    func lookupSFTPServer(_ error: NSErrorPointer) -> String {
        error?.pointee = TunnelFailure.unsupported as NSError
        return ""
    }
    func readSystemSSHHostKey(_ error: NSErrorPointer) -> String {
        error?.pointee = TunnelFailure.unsupported as NSError
        return ""
    }
    func usePlatformBridge() -> Bool { false }
    func createBridge(_ options: LibboxBridgeOptions?) throws -> LibboxBridgeSessionProtocol { throw TunnelFailure.unsupported }

    // Command server callbacks run in the extension, without a Flutter engine.
    func serviceStop() throws { provider?.stopFromCore() }
    func serviceReload() throws { throw TunnelFailure.unsupported }
    func getSystemProxyStatus() throws -> LibboxSystemProxyStatus {
        let value = LibboxSystemProxyStatus()
        lock.lock(); defer { lock.unlock() }
        value.available = networkSettings?.proxySettings?.httpServer != nil
        value.enabled = networkSettings?.proxySettings?.httpEnabled ?? false
        return value
    }
    func setSystemProxyEnabled(_ enabled: Bool) throws { throw TunnelFailure.unsupported }
    func triggerNativeCrash() throws { throw TunnelFailure.unsupported }
    func connectSSHAgent(_ ret0_: UnsafeMutablePointer<Int32>?) throws { throw TunnelFailure.unsupported }
    func writeDebugMessage(_ message: String?) {} // Raw upstream diagnostics can contain auth URLs.

    private func strings(_ iterator: LibboxStringIteratorProtocol?) -> [String] {
        guard let iterator else { return [] }
        var values: [String] = []
        while iterator.hasNext() { values.append(iterator.next()) }
        return values
    }

    private func prefixes(_ iterator: LibboxRoutePrefixIteratorProtocol?) -> [LibboxRoutePrefix] {
        guard let iterator else { return [] }
        var values: [LibboxRoutePrefix] = []
        while iterator.hasNext() { if let value = iterator.next() { values.append(value) } }
        return values
    }
}

private final class SettingsReply {
    private let lock = NSLock()
    private var value = false
    var failed: Bool { lock.lock(); defer { lock.unlock() }; return value }
    func set(_ error: Error?) { lock.lock(); value = error != nil; lock.unlock() }
}

private final class StringIterator: NSObject, LibboxStringIteratorProtocol {
    private let values: [String]
    private var index = 0
    init(_ values: [String]) { self.values = values }
    func hasNext() -> Bool { index < values.count }
    func next() -> String {
        guard hasNext() else { return "" }
        defer { index += 1 }
        return values[index]
    }
    func len() -> Int32 { Int32(values.count) }
}

private final class NetworkInterfaceIterator: NSObject, LibboxNetworkInterfaceIteratorProtocol {
    private let values: [LibboxNetworkInterface]
    private var index = 0
    init(_ values: [LibboxNetworkInterface]) { self.values = values }
    func hasNext() -> Bool { index < values.count }
    func next() -> LibboxNetworkInterface? {
        guard hasNext() else { return nil }
        defer { index += 1 }
        return values[index]
    }
}
