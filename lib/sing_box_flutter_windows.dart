import 'dart:async';
import 'dart:io';

import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';
import 'package:path/path.dart' as p;

import 'sing_box_flutter_platform_interface.dart';
import 'src/events.dart';
import 'src/windows_core_client.dart';

/// Windows backend using the bundled sing-box helper and Wintun driver.
///
/// The application must run elevated to create a Wintun adapter. The tunnel is
/// owned by the application process; cancelling a subscription does not stop it.
class SingBoxFlutterWindows extends SingBoxFlutterPlatform {
  /// Creates the Windows platform implementation.
  SingBoxFlutterWindows();

  /// Injects a process launcher and paths for transport/lifecycle tests.
  @visibleForTesting
  SingBoxFlutterWindows.withLauncher({
    required Future<Process> Function(String executable) launcher,
    required String workingPath,
    required String executable,
  })  : _launcher = launcher,
        _workingPath = workingPath,
        _executable = executable;

  static final _instance = SingBoxFlutterWindows();

  /// Called by Flutter's generated Dart plugin registrant.
  static void registerWith() {
    SingBoxFlutterPlatform.instance = _instance;
  }

  Future<Process> Function(String executable)? _launcher;
  String? _workingPath;
  String? _executable;
  Future<WindowsCoreClient>? _client;
  Future<int>? _retirement;
  int _generation = 0;
  int _statusRevision = 0;
  VpnStatus _status = const VpnStatus(state: VpnConnectionState.disconnected);
  final _updates = StreamController<SingBoxEvent>.broadcast();

  Future<WindowsCoreClient> _core() async {
    final existing = _client;
    if (existing != null) return existing;
    final generation = ++_generation;
    final connecting = _launch(generation);
    _client = connecting;
    try {
      return await connecting;
    } catch (_) {
      if (_generation == generation) _client = null;
      rethrow;
    }
  }

  Future<WindowsCoreClient> _launch(int generation) async {
    if (_launcher == null && !Platform.isWindows) {
      throw UnsupportedError('The Windows VPN backend requires Windows.');
    }
    final executable = _executable ??
        p.join(p.dirname(Platform.resolvedExecutable),
            'sing_box_flutter_core.exe');
    final Process process;
    try {
      // A broken transport can precede native route cleanup and lease release.
      // Keep single-flight launch ownership while waiting for actual termination.
      await _retirement?.timeout(const Duration(seconds: 60));
      _retirement = null;
      process = await (_launcher?.call(executable) ??
          Process.start(executable, const [],
              workingDirectory: p.dirname(executable)));
    } on TimeoutException {
      throw PlatformException(
        code: 'core_stopping',
        message: 'The previous Windows VPN core is still shutting down. '
            'Try initialization again after it exits.',
      );
    } catch (_) {
      throw PlatformException(
        code: 'core_unavailable',
        message: 'The bundled Windows VPN core could not be started. '
            'Build the Windows core and rebuild the application.',
      );
    }
    return WindowsCoreClient(process, onEvent: _emit, onFailure: () {
      if (_generation != generation) return;
      _retirement = process.exitCode;
      _client = null;
      _emit(const VpnStatus(
        state: VpnConnectionState.error,
        message: 'The Windows VPN core exited or stopped responding. '
            'Initialize again before reconnecting.',
      ));
    });
  }

  void _emit(SingBoxEvent event) {
    if (event is VpnStatus) {
      _status = event;
      _statusRevision++;
    }
    _updates.add(event);
  }

  Future<VpnStatus> _readStatus(WindowsCoreClient core, int generation,
      {bool notify = false}) async {
    final revision = _statusRevision;
    final result = await core.invoke('getStatus');
    if (result is! Map) {
      throw const FormatException('Malformed native VPN status.');
    }
    final status = VpnStatus.fromMap(Map<Object?, Object?>.from(result));
    // An RPC snapshot may resume after a newer event from the same pipe, or
    // after a replacement helper has taken ownership. Never replay older state.
    if (_generation == generation && _statusRevision == revision) {
      _status = status;
      _statusRevision++;
      if (notify) _updates.add(status);
    }
    return status;
  }

  Future<Object?> _invoke(String method,
          [Map<String, Object?>? arguments]) async =>
      (await _core()).invoke(method, arguments);

  @override
  Future<void> initialize({
    String? appGroup,
    String? providerBundleIdentifier,
    required String notificationTitle,
  }) async {
    var workingPath = _workingPath;
    if (workingPath == null) {
      final local = Platform.environment['LOCALAPPDATA'];
      if (local == null || local.isEmpty || !p.isAbsolute(local)) {
        throw PlatformException(
          code: 'initialization_failed',
          message: 'Windows local application storage is unavailable.',
        );
      }
      workingPath = p.join(
          local,
          p.basenameWithoutExtension(Platform.resolvedExecutable),
          'sing_box_flutter');
    }
    final pending = _core();
    final generation = _generation;
    final core = await pending;
    await core.invoke('initialize', {'workingPath': workingPath});
    await _readStatus(core, generation, notify: true);
  }

  @override
  Future<String> getVersion() async {
    final result = await _invoke('getVersion');
    if (result is! String || result.isEmpty) {
      throw StateError('The native sing-box version was not returned.');
    }
    return result;
  }

  @override
  Future<void> checkConfig(String config) async {
    await _invoke('checkConfig', {'config': config});
  }

  @override
  Future<bool> requestPermission() async =>
      await _invoke('requestPermission') == true;

  @override
  Future<void> start({required String config, required String name}) async {
    await _invoke('start', {'config': config, 'name': name});
  }

  @override
  Future<void> stop() async {
    await _invoke('stop');
  }

  @override
  Future<VpnStatus> getStatus() async {
    final pending = _core();
    final generation = _generation;
    return _readStatus(await pending, generation);
  }

  @override
  Stream<SingBoxEvent> get events => Stream<SingBoxEvent>.multi((controller) {
        final subscription = _updates.stream.listen(controller.add,
            onError: controller.addError, onDone: controller.close);
        controller.add(_status);
        controller.onCancel = subscription.cancel;
      }, isBroadcast: true);
}
