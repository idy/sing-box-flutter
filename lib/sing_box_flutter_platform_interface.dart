import 'package:plugin_platform_interface/plugin_platform_interface.dart';

import 'sing_box_flutter_method_channel.dart';
import 'src/events.dart';

/// Platform contract for native sing-box implementations.
abstract class SingBoxFlutterPlatform extends PlatformInterface {
  /// Constructs a platform implementation.
  SingBoxFlutterPlatform() : super(token: _token);

  static final Object _token = Object();
  static SingBoxFlutterPlatform _instance = MethodChannelSingBoxFlutter();

  /// The currently registered native implementation.
  static SingBoxFlutterPlatform get instance => _instance;

  /// Registers a platform implementation with token verification.
  static set instance(SingBoxFlutterPlatform instance) {
    PlatformInterface.verifyToken(instance, _token);
    _instance = instance;
  }

  /// Initializes native platform integration without connecting.
  Future<void> initialize({
    String? appGroup,
    String? providerBundleIdentifier,
    String? keychainAccessGroup,
    required String notificationTitle,
  }) =>
      throw UnimplementedError('initialize');

  /// Returns the linked native core version.
  Future<String> getVersion() => throw UnimplementedError('getVersion');

  /// Checks complete sing-box JSON without connecting.
  Future<void> checkConfig(String config) =>
      throw UnimplementedError('checkConfig');

  /// Requests platform VPN consent.
  Future<bool> requestPermission() =>
      throw UnimplementedError('requestPermission');

  /// Requests a native VPN connection.
  Future<void> start({required String config, required String name}) =>
      throw UnimplementedError('start');

  /// Stops the native VPN connection.
  Future<void> stop() => throw UnimplementedError('stop');

  /// Queries the current native connection state.
  Future<VpnStatus> getStatus() => throw UnimplementedError('getStatus');

  /// Broadcast native status and diagnostic events.
  Stream<SingBoxEvent> get events => throw UnimplementedError('events');
}
