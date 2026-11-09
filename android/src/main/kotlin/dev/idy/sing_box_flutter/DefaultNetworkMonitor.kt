/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Network selection follows SagerNet/sing-box-for-android DefaultNetworkListener.kt
 * and DefaultNetworkMonitor.kt at a548a7903e96266a0046018562c73b192edbfc18.
 * Copyright (C) 2022 by nekohasekai <contact-sagernet@sekai.icu>
 * Copyright (C) 2019 by Max Lv <max.c.lv@gmail.com>
 * Copyright (C) 2019 by Mygod Studio <contact-shadowsocks-android@mygod.be>
 * Distributed without warranty under GPL v3 or later; see android/LICENSE.
 */
package dev.idy.sing_box_flutter

import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.Build
import android.os.Handler
import android.os.HandlerThread
import io.nekohasekai.libbox.InterfaceUpdateListener
import java.net.NetworkInterface

internal class DefaultNetworkMonitor(private val service: SingBoxVpnService) {
    val connectivity: ConnectivityManager = service.getSystemService(ConnectivityManager::class.java)
    @Volatile var currentNetwork: Network? = null
        private set
    @Volatile private var listener: InterfaceUpdateListener? = null
    @Volatile private var running = false
    private var callbackThread: HandlerThread? = null
    private var handler: Handler? = null
    // The listener seed (on libbox's start thread) and connectivity callbacks (on
    // callbackThread) both publish. Each reads currentNetwork inside this lock and
    // delivers before releasing it, so a seed read before a newer network can never
    // be delivered after it: whichever publication runs last reads the newest path.
    // Delivery calls into libbox, which may call getInterfaces but never this lock.
    private val publication = Any()

    private val callback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) = dispatch { select(network) }
        override fun onLinkPropertiesChanged(network: Network, properties: LinkProperties) = dispatch {
            if (network == currentNetwork) publish()
        }
        override fun onCapabilitiesChanged(network: Network, capabilities: NetworkCapabilities) = dispatch {
            if (network == currentNetwork) publish()
        }
        override fun onLost(network: Network) = dispatch {
            if (network == currentNetwork) select(null)
        }
    }

    fun start() {
        check(!running) { "Network monitor is already running" }
        val thread = HandlerThread("sing-box-network").apply { start() }
        callbackThread = thread
        val callbackHandler = Handler(thread.looper)
        handler = callbackHandler
        running = true
        val request = NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_RESTRICTED)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)
            .build()
        try {
            // registerDefaultNetworkCallback returns the VPN itself on Android 9+.
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                connectivity.registerBestMatchingNetworkCallback(request, callback, callbackHandler)
            } else if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                connectivity.requestNetwork(request, callback, callbackHandler)
            } else {
                connectivity.requestNetwork(request, callback)
            }
        } catch (failure: Exception) {
            running = false
            thread.quitSafely()
            callbackThread = null
            handler = null
            throw failure
        }
        // Seed the initial path without ever choosing another VPN as the underlying network.
        connectivity.activeNetwork?.let { network ->
            if (connectivity.getNetworkCapabilities(network)?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN) == true) {
                currentNetwork = network
            }
        }
    }

    fun setListener(value: InterfaceUpdateListener?) {
        listener = value
        if (value != null) publish()
    }

    fun close() {
        listener = null
        if (running) {
            running = false
            try { connectivity.unregisterNetworkCallback(callback) } finally {
                currentNetwork = null
                handler?.removeCallbacksAndMessages(null)
                callbackThread?.quitSafely()
                callbackThread = null
                handler = null
            }
        }
    }

    private fun dispatch(block: () -> Unit) {
        handler?.post {
            if (running) {
                try {
                    block()
                } catch (_: Exception) {
                    // A broken monitor cannot safely leave the VPN advertised as connected.
                    NativeRuntime.worker.execute {
                        if (running && NativeRuntime.service === service) {
                            service.shutdownOnWorker("Android could not update the VPN network path.")
                        }
                    }
                }
            }
        }
    }

    private fun select(network: Network?) {
        if (!running) return
        currentNetwork = network
        // Updates Android's VPN underlay along with libbox's interface monitor.
        service.setUnderlyingNetworks(network?.let { arrayOf(it) } ?: emptyArray())
        publish()
    }

    /** Publishes the current path; see [publication] for the ordering guarantee. */
    private fun publish(attempt: Int = 0) {
        synchronized(publication) {
            val target = listener ?: return
            if (!running) return
            val network = currentNetwork
            if (network == null) {
                target.updateDefaultInterface("", -1, false, false)
                return
            }
            val properties = connectivity.getLinkProperties(network)
            val capabilities = connectivity.getNetworkCapabilities(network)
            val name = properties?.interfaceName
            val index = name?.let { runCatching { NetworkInterface.getByName(it)?.index }.getOrNull() }
            if (index == null || index <= 0 || capabilities == null) {
                // LinkProperties may arrive after onAvailable; retry off the lifecycle and UI threads.
                if (attempt < 10) handler?.postDelayed({
                    if (running && currentNetwork == network) publish(attempt + 1)
                }, 100)
                else target.updateDefaultInterface("", -1, false, false)
                return
            }
            target.updateDefaultInterface(name, index,
                !capabilities.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED), false)
        }
    }
}
