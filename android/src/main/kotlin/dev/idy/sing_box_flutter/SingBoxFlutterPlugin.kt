// SPDX-License-Identifier: GPL-3.0-or-later
package dev.idy.sing_box_flutter

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.net.VpnService
import io.flutter.embedding.engine.plugins.FlutterPlugin
import io.flutter.embedding.engine.plugins.activity.ActivityAware
import io.flutter.embedding.engine.plugins.activity.ActivityPluginBinding
import io.flutter.plugin.common.EventChannel
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import io.flutter.plugin.common.PluginRegistry
import io.nekohasekai.libbox.Libbox

class SingBoxFlutterPlugin : FlutterPlugin, MethodChannel.MethodCallHandler,
    EventChannel.StreamHandler, ActivityAware, PluginRegistry.ActivityResultListener {
    private lateinit var context: Context
    private lateinit var methods: MethodChannel
    private lateinit var events: EventChannel
    private var activityBinding: ActivityPluginBinding? = null
    private var permissionResult: MethodChannel.Result? = null
    private var statusListener: ((Map<String, String>) -> Unit)? = null

    override fun onAttachedToEngine(binding: FlutterPlugin.FlutterPluginBinding) {
        context = binding.applicationContext
        methods = MethodChannel(binding.binaryMessenger, "dev.idy.sing_box_flutter/methods")
        events = EventChannel(binding.binaryMessenger, "dev.idy.sing_box_flutter/events")
        methods.setMethodCallHandler(this)
        events.setStreamHandler(this)
    }

    override fun onMethodCall(call: MethodCall, result: MethodChannel.Result) {
        if (call.method == "requestPermission") {
            requestPermission(result)
            return
        }
        val failureMessage = when (call.method) {
            "initialize" -> "Could not initialize the native backend."
            "getVersion" -> "Could not load the native core."
            "checkConfig" -> "The configuration was rejected by the native core."
            "start" -> "Could not start the native VPN."
            "stop" -> "Could not stop the native VPN."
            "getStatus" -> "Could not read native status."
            else -> {
                result.notImplemented()
                return
            }
        }
        NativeRuntime.worker.execute {
            try {
                if (call.method != "getVersion" && call.method != "initialize") NativeRuntime.requireInitialized()
                val value: Any? = when (call.method) {
                    "initialize" -> {
                        NativeRuntime.initialize(context, requiredString(call, "notificationTitle"))
                        null
                    }
                    "getVersion" -> Libbox.version()
                    "checkConfig" -> {
                        NativeRuntime.checkConfig(requiredString(call, "config"))
                        null
                    }
                    "start" -> {
                        NativeRuntime.start(context, requiredString(call, "config"), requiredString(call, "name"))
                        null
                    }
                    "stop" -> {
                        NativeRuntime.stop { NativeRuntime.main.post { result.success(null) } }
                        return@execute
                    }
                    else -> NativeRuntime.snapshot()
                }
                NativeRuntime.main.post { result.success(value) }
            } catch (failure: NativeFailure) {
                NativeRuntime.main.post { result.error(failure.code, failure.message, null) }
            } catch (_: LinkageError) {
                NativeRuntime.main.post {
                    result.error("native_unavailable", "Cannot load libbox 1.14.4. Verify the AAR and device ABI.", null)
                }
            } catch (_: Exception) {
                NativeRuntime.main.post { result.error("native_error", failureMessage, null) }
            }
        }
    }

    private fun requiredString(call: MethodCall, key: String): String =
        (call.argument<Any>(key) as? String)?.takeIf { it.isNotBlank() }
            ?: throw NativeFailure("invalid_arguments", "A nonempty $key string is required.")

    private fun requestPermission(result: MethodChannel.Result) {
        try {
            NativeRuntime.requireInitialized()
            if (permissionResult != null) {
                throw NativeFailure("permission_in_progress", "A VPN permission request is already in progress.")
            }
            val intent = VpnService.prepare(context)
            if (intent == null) {
                result.success(true)
                return
            }
            val activity = activityBinding?.activity
                ?: throw NativeFailure("activity_required", "Attach a foreground Activity to request VPN permission.")
            permissionResult = result
            @Suppress("DEPRECATION")
            activity.startActivityForResult(intent, VPN_PERMISSION_REQUEST)
        } catch (failure: NativeFailure) {
            result.error(failure.code, failure.message, null)
        } catch (_: Exception) {
            permissionResult = null
            result.error("permission_failed", "Could not open Android VPN permission settings.", null)
        }
    }

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?): Boolean {
        if (requestCode != VPN_PERMISSION_REQUEST) return false
        val result = permissionResult ?: return false
        permissionResult = null
        val granted = resultCode == Activity.RESULT_OK && runCatching { VpnService.prepare(context) == null }.getOrDefault(false)
        result.success(granted)
        return true
    }

    override fun onListen(arguments: Any?, sink: EventChannel.EventSink) {
        onCancel(null)
        val listener: (Map<String, String>) -> Unit = { event ->
            if (statusListener != null) sink.success(event)
        }
        // The identity check prevents delivery of already queued events to an old subscription.
        lateinit var guarded: (Map<String, String>) -> Unit
        guarded = { event -> if (statusListener === guarded) listener(event) }
        statusListener = guarded
        NativeRuntime.subscribe(guarded)
    }

    override fun onCancel(arguments: Any?) {
        statusListener?.let { NativeRuntime.unsubscribe(it) }
        statusListener = null
    }

    override fun onAttachedToActivity(binding: ActivityPluginBinding) {
        activityBinding = binding
        binding.addActivityResultListener(this)
    }

    override fun onDetachedFromActivityForConfigChanges() {
        activityBinding?.removeActivityResultListener(this)
        activityBinding = null // Keep the outstanding result across Activity recreation.
    }

    override fun onReattachedToActivityForConfigChanges(binding: ActivityPluginBinding) = onAttachedToActivity(binding)

    override fun onDetachedFromActivity() {
        onDetachedFromActivityForConfigChanges()
        permissionResult?.success(false)
        permissionResult = null
    }

    override fun onDetachedFromEngine(binding: FlutterPlugin.FlutterPluginBinding) {
        onCancel(null)
        onDetachedFromActivity()
        methods.setMethodCallHandler(null)
        events.setStreamHandler(null)
    }

    private companion object {
        const val VPN_PERMISSION_REQUEST = 0x5342
    }
}
