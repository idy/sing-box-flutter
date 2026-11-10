/// Native sing-box VPN control for Android, iOS and Windows.
///
/// Install the native libbox artifacts and, on iOS, a Packet Tunnel extension
/// before using this library. See the package README for host setup.
library;

import 'dart:convert';

import 'sing_box_flutter_platform_interface.dart';
import 'src/events.dart';

export 'src/events.dart';

/// Controls the native VPN service independently of Flutter's UI lifecycle.
///
/// All instances refer to the same native VPN service. Cancelling an event
/// subscription does not stop the VPN; call [stop] explicitly.
class SingBoxFlutter {
  /// Initializes platform integration without starting a VPN connection.
  ///
  /// On iOS, [appGroup] must match the shared App Group entitlement and
  /// [providerBundleIdentifier] must identify the host's Packet Tunnel extension.
  /// Android uses [notificationTitle] for its foreground-service notification.
  Future<void> initialize({
    String? appGroup,
    String? providerBundleIdentifier,
    String notificationTitle = 'sing-box',
  }) {
    for (final value in [appGroup, providerBundleIdentifier]) {
      if (value != null && value.trim().isEmpty) {
        throw ArgumentError('Apple identifiers must not be empty.');
      }
    }
    if (notificationTitle.trim().isEmpty) {
      throw ArgumentError('The notification title must not be empty.');
    }
    return SingBoxFlutterPlatform.instance.initialize(
      appGroup: appGroup,
      providerBundleIdentifier: providerBundleIdentifier,
      notificationTitle: notificationTitle,
    );
  }

  /// The version of the libbox core actually linked into this application.
  Future<String> getVersion() => SingBoxFlutterPlatform.instance.getVersion();

  /// Checks sing-box JSON without connecting.
  ///
  /// Android and Windows validate it using the native engine. iOS performs structural and
  /// compiled-capability preflight in the app; the extension performs full engine
  /// validation during startup, so a successful preflight cannot guarantee start.
  /// Invalid JSON is rejected locally without including its contents in errors.
  Future<void> checkConfig(String config) {
    _validateConfig(config);
    return SingBoxFlutterPlatform.instance.checkConfig(config);
  }

  /// Requests the operating system's VPN permission.
  ///
  /// Returns false when consent is denied or cancelled. Android requires an
  /// attached Activity; iOS installs the configured VPN profile. Windows checks
  /// whether the application is elevated; it does not launch a UAC prompt.
  Future<bool> requestPermission() =>
      SingBoxFlutterPlatform.instance.requestPermission();

  /// Starts the native service with a complete sing-box JSON configuration.
  ///
  /// Completion means the request was accepted. Observe [status] for the actual
  /// connection outcome. Permission must be granted before starting.
  Future<void> start({required String config, String name = 'sing-box'}) {
    _validateConfig(config);
    if (name.trim().isEmpty || name.length > 128) {
      throw ArgumentError('The profile name must contain 1 to 128 characters.');
    }
    return SingBoxFlutterPlatform.instance.start(config: config, name: name);
  }

  /// Stops the native VPN service and releases its tunnel and core.
  Future<void> stop() => SingBoxFlutterPlatform.instance.stop();

  /// Queries native state, including after the Flutter UI has restarted.
  Future<VpnStatus> getStatus() => SingBoxFlutterPlatform.instance.getStatus();

  /// Broadcast native events. Each new native subscription receives its status.
  Stream<SingBoxEvent> get events => SingBoxFlutterPlatform.instance.events;

  /// Native connection-state changes. A UI subscription does not own the VPN.
  Stream<VpnStatus> get status =>
      events.where((event) => event is VpnStatus).cast<VpnStatus>();

  /// Optional sanitized diagnostics; availability depends on the native backend.
  Stream<SingBoxLog> get logs =>
      events.where((event) => event is SingBoxLog).cast<SingBoxLog>();

  static void _validateConfig(String config) {
    Object? decoded;
    try {
      decoded = jsonDecode(config);
    } on FormatException {
      throw const FormatException('Configuration must be valid sing-box JSON.');
    }
    if (decoded is! Map<String, dynamic>) {
      throw const FormatException('Configuration must be a JSON object.');
    }
  }
}
