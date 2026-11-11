import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:crypto/crypto.dart';

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
class SingBoxFlutterWindows extends SingBoxFlutterPlatform
    implements SingBoxFlutterApplicationIdentity {
  /// Creates the Windows platform implementation.
  SingBoxFlutterWindows()
      : _launchChannel = const MethodChannel('sing_box_flutter/windows_launch');

  /// Injects a process launcher and paths for transport/lifecycle tests.
  ///
  /// Omitting [launchChannel] bypasses native validation for fake processes only.
  /// Production instances always require the native secure-launch lease.
  @visibleForTesting
  SingBoxFlutterWindows.withLauncher({
    required Future<Process> Function(String executable) launcher,
    String? workingPath,
    String? localAppData,
    required String executable,
    MethodChannel? launchChannel,
  })  : _launcher = launcher,
        _workingPath = workingPath,
        _localAppData = localAppData,
        _executable = executable,
        _launchChannel = launchChannel;

  static final _instance = SingBoxFlutterWindows();

  /// Called by Flutter's generated Dart plugin registrant.
  static void registerWith() {
    SingBoxFlutterPlatform.instance = _instance;
  }

  /// Names storage by a caller-owned identifier, independent of install paths.
  @visibleForTesting
  static String applicationStorageName(String applicationId) {
    if (!RegExp(r'^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$')
        .hasMatch(applicationId)) {
      throw ArgumentError(
          'A stable application ID with 1 to 128 ASCII identifier characters is required.');
    }
    final readable = applicationId.replaceAll(RegExp(r'[^A-Za-z0-9_-]'), '_');
    final prefix =
        readable.substring(0, readable.length > 24 ? 24 : readable.length);
    final identity = sha256.convert(utf8.encode(applicationId));
    // v2 requires read and write isolation from lower-integrity processes.
    // Never adopt or relabel earlier unreleased storage protected only from writes.
    return 'v2-$prefix-$identity';
  }

  Future<Process> Function(String executable)? _launcher;
  final MethodChannel? _launchChannel;
  String? _workingPath;
  String? _localAppData;
  String? _executable;
  Future<WindowsCoreClient>? _client;
  Future<int>? _retirement;
  int _generation = 0;
  int? _initializedGeneration;
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

  bool get _isInitialized =>
      _client != null && _initializedGeneration == _generation;

  static PlatformException _notInitialized() => PlatformException(
        code: 'not_initialized',
        message: 'Initialize Windows VPN integration before using this method. '
            'Initialize again if the native core exited.',
      );

  Future<WindowsCoreClient> _initializedCore() async {
    if (!_isInitialized) throw _notInitialized();
    return _client!;
  }

  Future<void> _awaitRetirement() async {
    try {
      await _retirement?.timeout(const Duration(seconds: 60));
    } on TimeoutException {
      throw PlatformException(
        code: 'core_stopping',
        message: 'The previous Windows VPN core is still shutting down. '
            'Try again after it exits.',
      );
    }
  }

  Future<WindowsCoreClient> _launch(int generation) async {
    if (_launcher == null && !Platform.isWindows) {
      throw UnsupportedError('The Windows VPN backend requires Windows.');
    }
    // A broken transport can precede native route cleanup and lease release.
    // Keep single-flight launch ownership while waiting for actual termination.
    await _awaitRetirement();
    _retirement = null;
    final process = await _startVerifiedCore();
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

  static PlatformException _untrustedCore() => PlatformException(
        code: 'core_untrusted',
        message: 'The Windows VPN core could not be securely launched. '
            'Install the complete application in an administrator-managed '
            'location such as Program Files, then run it as administrator.',
      );

  static bool _isPinnedExecutablePath(String value) {
    // This exact Win32 device form reaches the verified NT volume through the
    // protected namespace links retained by native code. Bare GLOBALROOT can be
    // shadowed locally; accept only the image-path spelling selected natively.
    final root =
        RegExp(r'^\\\\\.\\Global\\GLOBALROOT\\Device\\HarddiskVolume[0-9]+\\')
            .firstMatch(value);
    if (root == null || value.length >= 260) return false;
    final parts = value.substring(root.end).split(r'\');
    if (parts.length < 2 || parts.last != 'sing_box_flutter_core.exe') {
      return false;
    }
    return parts.every((part) =>
        part.isNotEmpty &&
        part != '.' &&
        part != '..' &&
        !part.endsWith('.') &&
        !part.endsWith(' ') &&
        !RegExp(r'[/:*?"<>|\x00-\x1f]').hasMatch(part));
  }

  Future<Process> _startVerifiedCore() async {
    var executable = _executable ??
        p.join(p.dirname(Platform.resolvedExecutable),
            'sing_box_flutter_core.exe');
    final channel = _launchChannel;
    int? lease;
    if (channel != null) {
      try {
        final authorization =
            await channel.invokeMapMethod<String, Object?>('acquire');
        final id = authorization?['lease'];
        if (id is int && id > 0) lease = id;
        final nativePath = authorization?['executable'];
        if (lease == null ||
            nativePath is! String ||
            !_isPinnedExecutablePath(nativePath)) {
          throw _untrustedCore();
        }
        // Native code derives the device path from the open volume root and pins
        // its namespace links, directories and image through process creation.
        executable = nativePath;
      } catch (_) {
        if (lease != null) {
          try {
            await channel.invokeMethod<void>('release', lease);
          } catch (_) {
            // A failed release leaves the native lease pinned until process exit.
          }
        }
        throw _untrustedCore();
      }
    }
    Process? started;
    try {
      try {
        started = await (_launcher?.call(executable) ??
            Process.start(executable, const [],
                workingDirectory:
                    p.Context(style: p.Style.windows).dirname(executable)));
      } catch (_) {
        throw PlatformException(
          code: 'core_unavailable',
          message: 'The bundled Windows VPN core could not be started. '
              'Build the Windows core and rebuild the application.',
        );
      }
    } finally {
      if (channel != null && lease != null) {
        try {
          // Process.start completes after CreateProcess has mapped the image.
          // Every no-follow directory/file handle stays pinned until this point.
          await channel.invokeMethod<void>('release', lease);
        } catch (_) {
          if (started != null) {
            // Never attach a protocol client or send credentials on an uncertain
            // authorization result. Drain output and retire this unused child.
            unawaited(started.stdout.drain<void>().catchError((Object _) {}));
            unawaited(started.stderr.drain<void>().catchError((Object _) {}));
            unawaited(started.stdin.close().catchError((Object _) {}));
            _retirement = started.exitCode;
            started.kill();
          }
          throw _untrustedCore();
        }
      }
    }
    return started;
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
    return _status;
  }

  Future<Object?> _invokeInitialized(String method,
          [Map<String, Object?>? arguments]) async =>
      (await _initializedCore()).invoke(method, arguments);

  @override
  Future<void> initializeForApplication({
    required String applicationId,
    required String notificationTitle,
  }) =>
      initialize(
        applicationId: applicationId,
        notificationTitle: notificationTitle,
      );

  @override
  Future<void> initialize({
    String? applicationId,
    String? appGroup,
    String? providerBundleIdentifier,
    required String notificationTitle,
  }) async {
    if (applicationId == null) {
      throw ArgumentError(
          'Windows requires a stable applicationId when initializing the VPN.');
    }
    final storageName = applicationStorageName(applicationId);
    var workingPath = _workingPath;
    if (workingPath == null) {
      final paths = p.Context(style: p.Style.windows);
      final local = _localAppData ?? Platform.environment['LOCALAPPDATA'];
      if (local == null || local.isEmpty || !paths.isAbsolute(local)) {
        throw PlatformException(
          code: 'initialization_failed',
          message: 'Windows local application storage is unavailable.',
        );
      }
      workingPath = paths.join(local, storageName, 'sing_box_flutter');
    }
    final pending = _core();
    final generation = _generation;
    final core = await pending;
    await core.invoke('initialize', {'workingPath': workingPath});
    await _readStatus(core, generation, notify: true);
    if (_generation != generation || _client == null) {
      throw PlatformException(
        code: 'core_unavailable',
        message: 'The Windows VPN core exited during initialization.',
      );
    }
    _initializedGeneration = generation;
  }

  @override
  Future<String> getVersion() async {
    final result = await (await _core()).invoke('getVersion');
    if (result is! String || result.isEmpty) {
      throw StateError('The native sing-box version was not returned.');
    }
    return result;
  }

  @override
  Future<void> checkConfig(String config) async {
    await _invokeInitialized('checkConfig', {'config': config});
  }

  @override
  Future<bool> requestPermission() async =>
      await _invokeInitialized('requestPermission') == true;

  @override
  Future<void> start({required String config, required String name}) async {
    await _invokeInitialized('start', {'config': config, 'name': name});
  }

  @override
  Future<void> stop() async {
    if (!_isInitialized) {
      if (_initializedGeneration == null) throw _notInitialized();
      final generation = _generation;
      await _awaitRetirement();
      if (!_isInitialized) {
        if (_generation == generation) {
          _emit(const VpnStatus(state: VpnConnectionState.disconnected));
        }
        return;
      }
    }
    await _invokeInitialized('stop');
  }

  @override
  Future<VpnStatus> getStatus() async {
    if (!_isInitialized) {
      if (_initializedGeneration == null) throw _notInitialized();
      return _status;
    }
    final generation = _generation;
    try {
      return await _readStatus(await _initializedCore(), generation);
    } on PlatformException {
      if (!_isInitialized) return _status;
      rethrow;
    }
  }

  @override
  Stream<SingBoxEvent> get events => Stream<SingBoxEvent>.multi((controller) {
        final subscription = _updates.stream.listen(controller.add,
            onError: controller.addError, onDone: controller.close);
        controller.add(_status);
        controller.onCancel = subscription.cancel;
      }, isBroadcast: true);
}
