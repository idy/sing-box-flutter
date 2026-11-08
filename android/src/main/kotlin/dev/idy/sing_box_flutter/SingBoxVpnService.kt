/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * TUN and CommandServer integration adapted from SagerNet/sing-box-for-android
 * (VPNService.kt / BoxService.kt), commit a548a7903e96266a0046018562c73b192edbfc18.
 * Copyright (C) 2022 by nekohasekai <contact-sagernet@sekai.icu>
 * Distributed without warranty under GPL v3 or later; see android/LICENSE.
 */
package dev.idy.sing_box_flutter

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.IpPrefix
import android.net.VpnService
import android.os.Build
import android.os.ParcelFileDescriptor
import io.nekohasekai.libbox.CommandServer
import io.nekohasekai.libbox.CommandServerHandler
import io.nekohasekai.libbox.Libbox
import io.nekohasekai.libbox.OverrideOptions
import io.nekohasekai.libbox.SystemProxyStatus
import io.nekohasekai.libbox.TunOptions
import java.util.concurrent.atomic.AtomicBoolean

class SingBoxVpnService : VpnService(), CommandServerHandler {
    private var commandServer: CommandServer? = null // NativeRuntime.worker only.
    private lateinit var platform: AndroidPlatform
    private val fdLock = Any()
    private var tunnel: ParcelFileDescriptor? = null
    private val destroyed = AtomicBoolean(false)
    private val revoked = AtomicBoolean(false)
    private var sessionName = "sing-box"
    @Volatile private var latestStartId = 0

    override fun onCreate() {
        super.onCreate()
        NativeRuntime.service = this
        platform = AndroidPlatform(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        latestStartId = startId
        if (intent?.action == ACTION_STOP) {
            NativeRuntime.worker.execute { NativeRuntime.stop() }
            return START_NOT_STICKY
        }
        // Must enter foreground before waiting for the serialized Go startup.
        try {
            showNotification("Connecting")
        } catch (_: Exception) {
            NativeRuntime.invalidateStart()
            NativeRuntime.worker.execute { shutdownOnWorker("Android could not create the VPN foreground notification.") }
            return START_NOT_STICKY
        }
        val request = NativeRuntime.takeStart()
        NativeRuntime.worker.execute {
            if (request == null || !NativeRuntime.isCurrent(request) || destroyed.get() || revoked.get()) {
                if (commandServer == null) shutdownOnWorker()
            } else {
                startOnWorker(request)
            }
        }
        // No configuration is persisted in an Intent; process death must require an explicit new start.
        return START_NOT_STICKY
    }

    private fun startOnWorker(request: NativeRuntime.StartRequest) {
        try {
            NativeRuntime.requireInitialized()
            if (prepare(this) != null) throw NativeFailure("permission_required", "VPN permission was revoked.")
            if (commandServer != null) throw NativeFailure("busy", "A native VPN is already running.")
            sessionName = request.name
            platform.start()
            val server = CommandServer(this, platform)
            commandServer = server // Retain before start so every partial startup is cleaned up.
            server.start()
            server.startOrReloadService(request.config, OverrideOptions())
            if (server.needWIFIState()) {
                throw NativeFailure("unsupported", "Wi-Fi SSID rules require permissions and are not supported by this backend.")
            }
            if (synchronized(fdLock) { tunnel == null }) {
                throw NativeFailure("tun_failed", "The native core did not establish the Android VPN tunnel.")
            }
            if (destroyed.get() || revoked.get() || !NativeRuntime.isCurrent(request)) {
                shutdownOnWorker()
                return
            }
            showNotification("Connected")
            NativeRuntime.publish("connected")
        } catch (failure: NativeFailure) {
            shutdownOnWorker(failure.message)
        } catch (_: LinkageError) {
            shutdownOnWorker("Cannot load libbox 1.14.4 for this device ABI.")
        } catch (_: Exception) {
            shutdownOnWorker("The native core could not start the VPN. Check the configuration and Android VPN permission.")
        }
    }

    /** The sole owner of native teardown. Never called synchronously inside a Go callback. */
    internal fun shutdownOnWorker(error: String? = null) {
        if (NativeRuntime.service === this && NativeRuntime.snapshot()["state"] != "disconnected") {
            NativeRuntime.publish("disconnecting")
        }
        val server = commandServer
        commandServer = null
        var closeFailed = false
        try { server?.closeService() } catch (_: Exception) { closeFailed = true }
        try { server?.close() } catch (_: Exception) { closeFailed = true }
        try { platform.close() } catch (_: Exception) { closeFailed = true }
        synchronized(fdLock) {
            try { tunnel?.close() } catch (_: Exception) { closeFailed = true }
            tunnel = null
        }
        if (NativeRuntime.service === this) {
            val message = error ?: if (closeFailed) "The VPN stopped, but a native resource reported a cleanup failure." else null
            if (message == null) NativeRuntime.publish("disconnected") else NativeRuntime.publish("error", message)
        }
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelfResult(latestStartId)
    }

    override fun onRevoke() {
        revoked.set(true)
        NativeRuntime.invalidateStart()
        NativeRuntime.worker.execute { shutdownOnWorker("Android revoked VPN permission.") }
        // super.onRevoke() calls stopSelf(); teardown is serialized above and onDestroy below.
        super.onRevoke()
    }

    override fun onDestroy() {
        destroyed.set(true)
        NativeRuntime.worker.execute {
            // Idempotent cleanup also covers a host calling stopService directly.
            val state = NativeRuntime.snapshot()["state"]
            val preserveError = if (state == "error") NativeRuntime.snapshot()["message"] else null
            shutdownOnWorker(preserveError)
            if (NativeRuntime.service === this) NativeRuntime.service = null
        }
        super.onDestroy()
    }

    internal fun openTunnel(options: TunOptions): Int = synchronized(fdLock) {
        if (prepare(this) != null || revoked.get() || destroyed.get()) error("VPN permission is missing or revoked")
        check(tunnel == null) { "Only one Android TUN inbound is supported" }
        check(!options.isHTTPProxyEnabled) { "The optional Android HTTP proxy is not supported" }
        val builder = Builder().setSession(sessionName).setMtu(options.mtu)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) builder.setMetered(false)
        val ipv4 = options.inet4Address
        while (ipv4.hasNext()) ipv4.next().let { builder.addAddress(it.address(), it.prefix()) }
        val ipv6 = options.inet6Address
        while (ipv6.hasNext()) ipv6.next().let { builder.addAddress(it.address(), it.prefix()) }
        if (options.autoRoute) {
            if (options.dnsMode.value != Libbox.DNSModeDisabled) {
                val dns = options.dnsServerAddress
                while (dns.hasNext()) builder.addDnsServer(dns.next())
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                val routes4 = options.inet4RouteAddress
                if (!routes4.hasNext() && options.inet4Address.hasNext()) builder.addRoute("0.0.0.0", 0)
                while (routes4.hasNext()) routes4.next().let { builder.addRoute(it.address(), it.prefix()) }
                val routes6 = options.inet6RouteAddress
                if (!routes6.hasNext() && options.inet6Address.hasNext()) builder.addRoute("::", 0)
                while (routes6.hasNext()) routes6.next().let { builder.addRoute(it.address(), it.prefix()) }
                val excluded4 = options.inet4RouteExcludeAddress
                while (excluded4.hasNext()) excluded4.next().let { builder.excludeRoute(IpPrefix("${it.address()}/${it.prefix()}")) }
                val excluded6 = options.inet6RouteExcludeAddress
                while (excluded6.hasNext()) excluded6.next().let { builder.excludeRoute(IpPrefix("${it.address()}/${it.prefix()}")) }
            } else {
                // libbox computes the route complement for Android versions without excludeRoute.
                val routes4 = options.inet4RouteRange
                while (routes4.hasNext()) routes4.next().let { builder.addRoute(it.address(), it.prefix()) }
                val routes6 = options.inet6RouteRange
                while (routes6.hasNext()) routes6.next().let { builder.addRoute(it.address(), it.prefix()) }
            }
        }
        val include = options.includePackage
        val exclude = options.excludePackage
        check(!(include.hasNext() && exclude.hasNext())) { "Cannot combine allowed and excluded applications" }
        // Missing package names fail startup rather than silently changing the requested routing policy.
        while (include.hasNext()) builder.addAllowedApplication(include.next())
        while (exclude.hasNext()) builder.addDisallowedApplication(exclude.next())
        builder.setUnderlyingNetworks(platform.networkMonitor.currentNetwork?.let { arrayOf(it) })
        val descriptor = builder.establish() ?: error("Android refused to establish the VPN tunnel")
        tunnel = descriptor
        // libbox duplicates this descriptor; Kotlin retains and closes the original after the core stops.
        descriptor.fd
    }

    private fun showNotification(state: String) {
        val manager = getSystemService(NotificationManager::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            manager.createNotificationChannel(NotificationChannel(CHANNEL_ID, "VPN connection", NotificationManager.IMPORTANCE_LOW))
        }
        val builder = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) Notification.Builder(this, CHANNEL_ID)
            else @Suppress("DEPRECATION") Notification.Builder(this)
        val stop = PendingIntent.getService(this, 0, Intent(this, SingBoxVpnService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
        builder.setSmallIcon(android.R.drawable.stat_sys_warning)
            .setContentTitle(NativeRuntime.notificationTitle)
            .setContentText(state).setCategory(Notification.CATEGORY_SERVICE)
            .setOngoing(true).setOnlyAlertOnce(true)
            .addAction(Notification.Action.Builder(null, "Disconnect", stop).build())
        packageManager.getLaunchIntentForPackage(packageName)?.let { launch ->
            builder.setContentIntent(PendingIntent.getActivity(this, 1, launch, PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE))
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(NOTIFICATION_ID, builder.build(), ServiceInfo.FOREGROUND_SERVICE_TYPE_SYSTEM_EXEMPTED)
        } else startForeground(NOTIFICATION_ID, builder.build())
    }

    override fun serviceStop() { NativeRuntime.worker.execute { NativeRuntime.stop() } }
    override fun serviceReload(): Unit = throw UnsupportedOperationException("Reload is unsupported; use stop and start")
    override fun getSystemProxyStatus() = SystemProxyStatus().apply { available = false; enabled = false }
    override fun setSystemProxyEnabled(enabled: Boolean): Unit = throw UnsupportedOperationException("System proxy is unsupported")
    override fun triggerNativeCrash(): Unit = throw UnsupportedOperationException("Native crash requests are disabled")
    override fun writeDebugMessage(message: String?) { /* Never forward unredacted native debug output. */ }
    override fun connectSSHAgent(): Int = throw UnsupportedOperationException("SSH agent integration is unsupported")

    companion object {
        internal const val ACTION_START = "dev.idy.sing_box_flutter.START"
        private const val ACTION_STOP = "dev.idy.sing_box_flutter.STOP"
        private const val CHANNEL_ID = "dev.idy.sing_box_flutter.vpn"
        private const val NOTIFICATION_ID = 0x5342
    }
}
