/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Network-bound resolver follows SagerNet/sing-box-for-android LocalResolver.kt
 * at a548a7903e96266a0046018562c73b192edbfc18.
 * Copyright (C) 2022 by nekohasekai <contact-sagernet@sekai.icu>
 * Distributed without warranty under GPL v3 or later; see android/LICENSE.
 */
package dev.idy.sing_box_flutter

import android.annotation.TargetApi
import android.net.DnsResolver
import android.os.Build
import android.os.CancellationSignal
import android.system.ErrnoException
import android.system.OsConstants
import io.nekohasekai.libbox.ExchangeContext
import io.nekohasekai.libbox.LocalDNSTransport
import java.net.Inet4Address
import java.net.Inet6Address
import java.net.UnknownHostException
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executor
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

internal class PlatformDNS(private val monitor: DefaultNetworkMonitor) : LocalDNSTransport {
    override fun raw() = Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q

    @TargetApi(Build.VERSION_CODES.Q)
    override fun exchange(ctx: ExchangeContext, message: ByteArray) {
        check(Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) { "Raw system DNS requires Android 10 or later" }
        val network = monitor.currentNetwork ?: error("No physical network is available for DNS")
        val signal = CancellationSignal()
        val done = CountDownLatch(1)
        val completed = AtomicBoolean(false)
        fun complete(block: () -> Unit) {
            if (completed.compareAndSet(false, true)) {
                try { block() } finally { done.countDown() }
            }
        }
        ctx.onCancel { complete { signal.cancel(); ctx.errnoCode(OsConstants.ECANCELED) } }
        DnsResolver.getInstance().rawQuery(network, message, DnsResolver.FLAG_NO_RETRY,
            Executor { it.run() }, signal, object : DnsResolver.Callback<ByteArray> {
                override fun onAnswer(answer: ByteArray, rcode: Int) = complete {
                    if (rcode == 0) ctx.rawSuccess(answer) else ctx.errorCode(rcode)
                }
                override fun onError(error: DnsResolver.DnsException) = complete {
                    ctx.errnoCode((error.cause as? ErrnoException)?.errno ?: OsConstants.EIO)
                }
            })
        try {
            if (!done.await(30, TimeUnit.SECONDS)) {
                complete { signal.cancel(); ctx.errnoCode(OsConstants.ETIMEDOUT) }
            }
        } catch (_: InterruptedException) {
            complete { signal.cancel(); ctx.errnoCode(OsConstants.ECANCELED) }
            Thread.currentThread().interrupt()
        }
    }

    override fun lookup(ctx: ExchangeContext, network: String, domain: String) {
        val physical = monitor.currentNetwork ?: error("No physical network is available for DNS")
        try {
            val addresses = physical.getAllByName(domain).filter {
                when (network) {
                    "ip4" -> it is Inet4Address
                    "ip6" -> it is Inet6Address
                    "ip" -> true
                    else -> throw UnsupportedOperationException("Unsupported DNS address family")
                }
            }
            ctx.success(addresses.mapNotNull { it.hostAddress }.joinToString("\n"))
        } catch (_: UnknownHostException) {
            ctx.errorCode(3) // NXDOMAIN. Do not expose the queried name in an exception.
        }
    }
}
