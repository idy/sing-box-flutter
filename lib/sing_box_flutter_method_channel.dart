import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';

import 'sing_box_flutter_platform_interface.dart';
import 'src/events.dart';

/// Uses the native Android or iOS sing-box method and event channels.
/// Windows registers its own pipe-based implementation of the same platform API.
class MethodChannelSingBoxFlutter extends SingBoxFlutterPlatform {
  /// Shared control channel.
  @visibleForTesting
  final methodChannel = const MethodChannel('dev.idy.sing_box_flutter/methods');

  /// Shared native lifecycle event channel.
  @visibleForTesting
  final eventChannel = const EventChannel('dev.idy.sing_box_flutter/events');

  late final Stream<SingBoxEvent> _events =
      eventChannel.receiveBroadcastStream().map((event) {
    if (event is! Map) {
      throw const FormatException('Malformed native event.');
    }
    return SingBoxEvent.fromMap(Map<Object?, Object?>.from(event));
  });

  @override
  Future<void> initialize({
    String? applicationId,
    String? appGroup,
    String? providerBundleIdentifier,
    required String notificationTitle,
  }) =>
      methodChannel.invokeMethod<void>('initialize', {
        if (appGroup != null) 'appGroup': appGroup,
        if (providerBundleIdentifier != null)
          'providerBundleIdentifier': providerBundleIdentifier,
        'notificationTitle': notificationTitle,
      });

  @override
  Future<String> getVersion() async {
    final value = await methodChannel.invokeMethod<String>('getVersion');
    if (value == null || value.isEmpty) {
      throw StateError('The native sing-box version was not returned.');
    }
    return value;
  }

  @override
  Future<void> checkConfig(String config) =>
      methodChannel.invokeMethod<void>('checkConfig', {'config': config});

  @override
  Future<bool> requestPermission() async =>
      await methodChannel.invokeMethod<bool>('requestPermission') ?? false;

  @override
  Future<void> start({required String config, required String name}) =>
      methodChannel
          .invokeMethod<void>('start', {'config': config, 'name': name});

  @override
  Future<void> stop() => methodChannel.invokeMethod<void>('stop');

  @override
  Future<VpnStatus> getStatus() async {
    final value = await methodChannel.invokeMapMethod<Object?, Object?>(
      'getStatus',
    );
    if (value == null) {
      throw StateError('The native VPN status was not returned.');
    }
    return VpnStatus.fromMap(value);
  }

  @override
  Stream<SingBoxEvent> get events => _events;
}
