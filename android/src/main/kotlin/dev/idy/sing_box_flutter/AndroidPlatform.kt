/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Interface enumeration / connection ownership adapted from SagerNet/sing-box-for-android
 * PlatformInterfaceWrapper.kt at a548a7903e96266a0046018562c73b192edbfc18.
 * Copyright (C) 2022 by nekohasekai <contact-sagernet@sekai.icu>
 * Distributed without warranty under GPL v3 or later; see android/LICENSE.
 * Implements only experimental/libbox/platform.go from sing-box v1.14.4.
 */
package dev.idy.sing_box_flutter

import android.net.NetworkCapabilities
import android.os.Build
import android.os.Process
import android.provider.Settings
import android.system.OsConstants
import io.nekohasekai.libbox.BridgeOptions
import io.nekohasekai.libbox.BridgeSession
import io.nekohasekai.libbox.ConnectionOwner
import io.nekohasekai.libbox.InterfaceUpdateListener
import io.nekohasekai.libbox.Libbox
import io.nekohasekai.libbox.LocalDNSTransport
import io.nekohasekai.libbox.NeighborUpdateListener
import io.nekohasekai.libbox.NetworkInterfaceIterator
import io.nekohasekai.libbox.Notification
import io.nekohasekai.libbox.PlatformInterface
import io.nekohasekai.libbox.PlatformUser
import io.nekohasekai.libbox.ShellSession
import io.nekohasekai.libbox.StringIterator
import io.nekohasekai.libbox.TunOptions
import io.nekohasekai.libbox.WIFIState
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.NetworkInterface
import io.nekohasekai.libbox.NetworkInterface as BoxNetworkInterface

internal class AndroidPlatform(private val service: SingBoxVpnService) : PlatformInterface {
    val networkMonitor = DefaultNetworkMonitor(service)
    private val resolver = PlatformDNS(networkMonitor)
    fun start() = networkMonitor.start()
    fun close() = networkMonitor.close()

    override fun usePlatformAutoDetectInterfaceControl() = true
    override fun autoDetectInterfaceControl(fd: Int) {
        check(service.protect(fd)) { "Android could not protect an outbound socket" }
    }
    override fun openTun(options: TunOptions): Int = service.openTunnel(options)
    override fun localDNSTransport(): LocalDNSTransport = resolver
    override fun startDefaultInterfaceMonitor(listener: InterfaceUpdateListener) = networkMonitor.setListener(listener)
    override fun closeDefaultInterfaceMonitor(listener: InterfaceUpdateListener?) = networkMonitor.setListener(null)

    override fun getInterfaces(): NetworkInterfaceIterator {
        val result = mutableListOf<BoxNetworkInterface>()
        val connectivity = networkMonitor.connectivity
        for (network in connectivity.allNetworks) {
            val properties = connectivity.getLinkProperties(network) ?: continue
            val capabilities = connectivity.getNetworkCapabilities(network) ?: continue
            val name = properties.interfaceName ?: continue
            val device = NetworkInterface.getByName(name) ?: continue
            val item = BoxNetworkInterface()
            item.name = name
            item.index = device.index
            item.mtu = device.mtu
            item.addresses = NativeStrings(device.interfaceAddresses.map { address ->
                "${InetAddress.getByAddress(address.address.address).hostAddress}/${address.networkPrefixLength}"
            })
            item.dnsServer = NativeStrings(properties.dnsServers.mapNotNull { it.hostAddress })
            item.gateway = NativeStrings(properties.routes.filter { it.destination.prefixLength == 0 }
                .mapNotNull { it.gateway }.filterNot { it.isAnyLocalAddress }.mapNotNull { it.hostAddress })
            item.type = when {
                capabilities.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> Libbox.InterfaceTypeWIFI
                capabilities.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> Libbox.InterfaceTypeCellular
                capabilities.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> Libbox.InterfaceTypeEthernet
                else -> Libbox.InterfaceTypeOther
            }
            var flags = 0
            if (device.isUp) flags = flags or OsConstants.IFF_UP or OsConstants.IFF_RUNNING
            if (device.isLoopback) flags = flags or OsConstants.IFF_LOOPBACK
            if (device.isPointToPoint) flags = flags or OsConstants.IFF_POINTOPOINT
            if (device.supportsMulticast()) flags = flags or OsConstants.IFF_MULTICAST
            item.flags = flags
            item.metered = !capabilities.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED)
            result.add(item)
        }
        val iterator = result.iterator()
        return object : NetworkInterfaceIterator {
            override fun hasNext() = iterator.hasNext()
            override fun next(): BoxNetworkInterface = iterator.next()
        }
    }

    override fun findConnectionOwner(ipProtocol: Int, sourceAddress: String, sourcePort: Int,
        destinationAddress: String, destinationPort: Int): ConnectionOwner {
        val owner = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            val uid = networkMonitor.connectivity.getConnectionOwnerUid(ipProtocol,
                InetSocketAddress(sourceAddress, sourcePort), InetSocketAddress(destinationAddress, destinationPort))
            check(uid != Process.INVALID_UID) { "Connection owner was not found" }
            ConnectionOwner().apply { userId = uid }
        } else Libbox.findConnectionOwner(ipProtocol, sourceAddress, sourcePort, destinationAddress, destinationPort)
        val packages = service.packageManager.getPackagesForUid(owner.userId)?.toList().orEmpty()
        owner.userName = packages.firstOrNull().orEmpty()
        owner.setAndroidPackageNames(NativeStrings(packages))
        return owner
    }

    override fun underNetworkExtension() = false
    override fun includeAllNetworks() = false
    override fun readWIFIState(): WIFIState? = null // No location/SSID permissions; startup rejects SSID rules.
    override fun clearDNSCache() { /* DNS resolution is bound to the current physical Network. */ }
    override fun registerMyInterface(name: String?) { /* libbox owns its interface registry. */ }
    override fun tailscaleHostname(): String = Settings.Global.getString(service.contentResolver, "device_name")
        ?.takeIf { it.isNotBlank() } ?: "${Build.MANUFACTURER} ${Build.MODEL}"

    override fun sendNotification(notification: Notification?): Unit = unsupported("Core notifications")
    override fun cancelNotification(identifier: String?, typeID: Int): Unit = unsupported("Core notifications")
    override fun startNeighborMonitor(listener: NeighborUpdateListener?): Unit = unsupported("Neighbor monitoring")
    override fun closeNeighborMonitor(listener: NeighborUpdateListener?) { /* No neighbor monitor was allocated. */ }
    override fun usePlatformShell() = false
    override fun checkPlatformShell(): Unit = unsupported("Platform shell")
    override fun openShellSession(user: PlatformUser?, command: String?, environ: StringIterator?,
        term: String?, rows: Int, cols: Int): ShellSession = unsupported("Platform shell")
    override fun lookupUser(username: String?): PlatformUser = unsupported("User lookup")
    override fun lookupSFTPServer(): String = unsupported("SFTP")
    override fun readSystemSSHHostKey(): String = unsupported("System SSH host keys")
    override fun usePlatformBridge() = false
    override fun createBridge(options: BridgeOptions?): BridgeSession = unsupported("Platform bridges")

    private fun unsupported(capability: String): Nothing = throw UnsupportedOperationException("$capability is unsupported on Android")
}

/** gomobile iterators are consumable; len reports the remaining elements. */
internal class NativeStrings(private val values: List<String>) : StringIterator {
    private var offset = 0
    override fun len(): Int = values.size - offset
    override fun hasNext(): Boolean = offset < values.size
    override fun next(): String = if (hasNext()) values[offset++] else ""
}
