// SPDX-License-Identifier: GPL-3.0-or-later
package dev.idy.sing_box_flutter

import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.os.Handler
import android.os.Looper
import io.nekohasekai.libbox.Libbox
import io.nekohasekai.libbox.SetupOptions
import java.io.File
import java.security.SecureRandom
import java.util.concurrent.Executors

internal class NativeFailure(val code: String, override val message: String) : Exception(message)

/** Process-owned state: neither cancelling events nor detaching Flutter stops the VPN. */
internal object NativeRuntime {
    const val CORE_VERSION = "1.14.4"
    val worker = Executors.newSingleThreadExecutor { runnable ->
        Thread(runnable, "sing-box-lifecycle")
    }
    val main = Handler(Looper.getMainLooper())
    private val lock = Any()
    private val listeners = mutableSetOf<(Map<String, String>) -> Unit>()
    private var status = statusEvent("disconnected")
    private var pendingStart: StartRequest? = null
    private var generation = 0L
    @Volatile var initialized = false
        private set
    @Volatile var notificationTitle = "sing-box"
        private set
    @Volatile var service: SingBoxVpnService? = null

    data class StartRequest(val generation: Long, val config: String, val name: String)

    fun initialize(context: Context, title: String) {
        if (!initialized) {
            if (Libbox.version() != CORE_VERSION) {
                throw NativeFailure("core_version_mismatch", "Expected libbox $CORE_VERSION; rebuild the pinned native artifact.")
            }
            val root = File(context.noBackupFilesDir, "sing_box_flutter")
            val base = File(root, "base")
            val work = File(root, "working")
            val temp = File(context.cacheDir, "sing_box_flutter")
            listOf(base, work, temp).forEach {
                if (!it.isDirectory && !it.mkdirs()) {
                    throw NativeFailure("initialize_failed", "Could not create private native directories.")
                }
            }
            Libbox.setup(SetupOptions().apply {
                basePath = base.absolutePath
                workingPath = work.absolutePath
                tempPath = temp.absolutePath
                fixAndroidStack = true
                commandServerListenPort = 0 // Private Unix socket, never a public TCP listener.
                commandServerSecret = ByteArray(32).also { SecureRandom().nextBytes(it) }
                    .joinToString("") { "%02x".format(it.toInt() and 0xff) }
                logMaxLines = 100
                debug = false
                crashReportSource = "android"
            })
            initialized = true
        }
        notificationTitle = title.take(80)
    }

    fun requireInitialized() {
        if (!initialized) throw NativeFailure("not_initialized", "Call initialize before using the native backend.")
    }

    fun checkConfig(config: String) {
        try {
            Libbox.checkConfig(config)
        } catch (_: Exception) {
            // Native parse errors can echo credentials and fragments of configuration.
            throw NativeFailure("invalid_config", "The configuration was rejected by libbox $CORE_VERSION.")
        }
    }

    /** Called only on worker. Intent deliberately contains no configuration or credentials. */
    fun start(context: Context, config: String, name: String) {
        val state = snapshot()["state"]
        if (service != null || (state != "disconnected" && state != "error")) {
            throw NativeFailure("busy", "Stop the existing VPN before starting another configuration.")
        }
        if (VpnService.prepare(context) != null) {
            throw NativeFailure("permission_required", "VPN permission is required before starting.")
        }
        checkConfig(config)
        if (!Libbox.hasTunInbound(config)) {
            throw NativeFailure("tun_required", "An Android VPN configuration must contain a TUN inbound.")
        }
        synchronized(lock) {
            generation++
            pendingStart = StartRequest(generation, config, name.take(80))
        }
        publish("connecting")
        try {
            val intent = Intent(context, SingBoxVpnService::class.java).setAction(SingBoxVpnService.ACTION_START)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) context.startForegroundService(intent)
            else context.startService(intent)
        } catch (_: Exception) {
            invalidateStart()
            publish("error", "Android could not start the foreground VPN service. Start it while the app is visible.")
            throw NativeFailure("start_failed", "Android could not start the foreground VPN service.")
        }
    }

    fun takeStart(): StartRequest? = synchronized(lock) {
        pendingStart.also { pendingStart = null }
    }

    fun isCurrent(request: StartRequest): Boolean = synchronized(lock) { generation == request.generation }

    fun invalidateStart() = synchronized(lock) {
        generation++
        pendingStart = null
    }

    /** Called only on worker; channel completion also waits for Android service destruction. */
    fun stop(onStopped: (() -> Unit)? = null) {
        invalidateStart()
        val current = service
        if (current == null) {
            publish("disconnected")
            onStopped?.invoke()
        } else {
            current.requestStopOnWorker(onStopped)
        }
    }

    fun snapshot(): Map<String, String> = synchronized(lock) { status.toMap() }

    fun publish(state: String, message: String? = null) = synchronized(lock) {
        status = statusEvent(state, message)
        val event = status.toMap()
        listeners.toList().forEach { listener -> main.post { listener(event) } }
    }

    fun subscribe(listener: (Map<String, String>) -> Unit) = synchronized(lock) {
        listeners.add(listener)
        val current = status.toMap()
        main.post { listener(current) }
    }

    fun unsubscribe(listener: (Map<String, String>) -> Unit) = synchronized(lock) {
        listeners.remove(listener)
        Unit
    }

    private fun statusEvent(state: String, message: String? = null): Map<String, String> =
        buildMap {
            put("type", "status")
            put("state", state)
            if (message != null) put("message", message.take(240))
        }
}
