// SPDX-License-Identifier: GPL-3.0-or-later
package dev.idy.sing_box_flutter

import android.os.Looper
import io.flutter.plugin.common.EventChannel
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.Shadows.shadowOf
import org.robolectric.annotation.Config
import java.util.concurrent.TimeUnit

/** These tests cover channel/lifecycle state without substituting a fake native core. */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [35], manifest = Config.NONE)
class SingBoxFlutterPluginTest {
    private val plugin = SingBoxFlutterPlugin()

    @After
    fun cleanUp() {
        plugin.onCancel(null)
        NativeRuntime.worker.submit { NativeRuntime.stop() }.get(5, TimeUnit.SECONDS)
        shadowOf(Looper.getMainLooper()).idle()
    }

    @Test
    fun methodsRequireInitializationIncludingPermissionAndStatus() {
        listOf("getStatus", "checkConfig", "start", "stop", "requestPermission").forEach { method ->
            val result = RecordingResult()
            plugin.onMethodCall(MethodCall(method, null), result)
            NativeRuntime.worker.submit {}.get(5, TimeUnit.SECONDS)
            shadowOf(Looper.getMainLooper()).idle()
            assertEquals("$method must not run before initialize", "not_initialized", result.errorCode)
            assertFalse(result.succeeded)
        }
    }

    @Test
    fun unknownMethodsAreNotImplemented() {
        val result = RecordingResult()
        plugin.onMethodCall(MethodCall("unknown", null), result)
        assertTrue(result.notImplemented)
    }

    @Test
    fun subscriberReceivesCurrentStateAndCancellationDoesNotDisconnect() {
        NativeRuntime.publish("connected")
        val first = RecordingSink()
        plugin.onListen(null, first)
        shadowOf(Looper.getMainLooper()).idle()
        assertEquals("connected", first.events.single()["state"])
        plugin.onCancel(null)
        assertEquals("connected", NativeRuntime.snapshot()["state"])
        NativeRuntime.publish("disconnecting")
        shadowOf(Looper.getMainLooper()).idle()
        assertEquals(1, first.events.size)
    }

    @Test
    fun queuedEventsCannotReachCancelledOrReplacementSubscriptions() {
        val first = RecordingSink()
        val second = RecordingSink()
        plugin.onListen(null, first)
        NativeRuntime.publish("connecting")
        plugin.onCancel(null)
        plugin.onListen(null, second)
        shadowOf(Looper.getMainLooper()).idle()
        assertTrue(first.events.isEmpty())
        assertEquals(listOf("connecting"), second.events.map { it["state"] })
    }

    @Test
    fun stopWithoutAServiceIsIdempotentAndInvalidatesOlderStarts() {
        val request = NativeRuntime.StartRequest(0, "not a configuration", "test")
        NativeRuntime.worker.submit {
            NativeRuntime.stop()
            NativeRuntime.stop()
        }.get(5, TimeUnit.SECONDS)
        assertFalse(NativeRuntime.isCurrent(request))
        assertEquals("disconnected", NativeRuntime.snapshot()["state"])
    }

    @Test
    fun everyStopCompletionWaitsForServiceDestructionAndRunsOnce() {
        val controller = Robolectric.buildService(SingBoxVpnService::class.java).create()
        val service = controller.get()
        var completed = 0
        try {
            NativeRuntime.worker.submit {
                NativeRuntime.stop { completed++ }
                NativeRuntime.stop { completed++ }
            }.get(5, TimeUnit.SECONDS)
            assertEquals(0, completed)
            assertTrue(shadowOf(service).isStoppedBySelf)
            // Android must destroy the instance before a new VPN can be accepted.
            assertTrue(NativeRuntime.service === service)
        } finally {
            controller.destroy()
            NativeRuntime.worker.submit {}.get(5, TimeUnit.SECONDS)
        }
        assertEquals(2, completed)
        assertEquals(null, NativeRuntime.service)
        NativeRuntime.worker.submit { NativeRuntime.stop() }.get(5, TimeUnit.SECONDS)
        assertEquals(2, completed)
    }

    @Test
    fun gomobileStringIteratorTracksRemainingValues() {
        val strings = NativeStrings(listOf("one", "two"))
        assertEquals(2, strings.len())
        assertEquals("one", strings.next())
        assertEquals(1, strings.len())
        assertEquals("two", strings.next())
        assertEquals(0, strings.len())
        assertFalse(strings.hasNext())
        assertEquals("", strings.next())
    }

    private class RecordingResult : MethodChannel.Result {
        var errorCode: String? = null
        var succeeded = false
        var notImplemented = false
        override fun success(result: Any?) { succeeded = true }
        override fun error(errorCode: String, errorMessage: String?, errorDetails: Any?) { this.errorCode = errorCode }
        override fun notImplemented() { notImplemented = true }
    }

    private class RecordingSink : EventChannel.EventSink {
        val events = mutableListOf<Map<*, *>>()
        override fun success(event: Any?) { events.add(event as Map<*, *>) }
        override fun error(code: String, message: String?, details: Any?) = Unit
        override fun endOfStream() = Unit
    }
}
