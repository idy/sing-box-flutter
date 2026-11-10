import 'dart:async';
import 'dart:convert';
import 'dart:ffi' as ffi;
import 'dart:io';

import 'package:crypto/crypto.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';
import 'package:path/path.dart' as p;

import 'sing_box_flutter_platform_interface.dart';
import 'src/events.dart';

/// Resolves and validates the stable application identity supplied by the host.
@visibleForTesting
String linuxApplicationId({
  String? applicationIdOverride,
  String? Function()? readApplicationId,
}) {
  try {
    return _validatedApplicationId(
        applicationIdOverride ?? (readApplicationId ?? _gio.applicationId)());
  } catch (_) {
    throw _applicationIdentityError();
  }
}

PlatformException _applicationIdentityError() => PlatformException(
      code: 'application_identity',
      message: 'A valid GTK application ID or '
          'SING_BOX_FLUTTER_APPLICATION_ID build define is required.',
    );

String _validatedApplicationId(String? applicationId) {
  // GApplication/DBus IDs: ASCII elements, no leading digits, at least one dot.
  if (applicationId == null ||
      applicationId.length > 255 ||
      !RegExp(r'^[A-Za-z_-][A-Za-z0-9_-]*(\.[A-Za-z_-][A-Za-z0-9_-]*)+$')
          .hasMatch(applicationId)) {
    throw _applicationIdentityError();
  }
  return applicationId;
}

/// Resolves Linux state storage using the stable host application identity.
///
/// With no valid data home, allocates a private directory for this backend. This
/// fallback survives helper restarts but does not persist across engine launches.
@visibleForTesting
String linuxDataDirectory({
  required Map<String, String> environment,
  required String applicationId,
  required String temporaryDirectory,
  void Function(String)? onTemporaryDirectoryCreated,
}) {
  _validatedApplicationId(applicationId);
  final xdgDataHome = environment['XDG_DATA_HOME'];
  final home = environment['HOME'];
  final String dataHome;
  if (xdgDataHome != null && p.posix.isAbsolute(xdgDataHome)) {
    dataHome = xdgDataHome;
  } else if (home != null && p.posix.isAbsolute(home)) {
    dataHome = p.posix.join(home, '.local', 'share');
  } else {
    if (!p.posix.isAbsolute(temporaryDirectory)) {
      throw ArgumentError('The fallback state directory must be absolute.');
    }
    try {
      return _privateTemporaryDirectory(
          temporaryDirectory, onTemporaryDirectoryCreated);
    } catch (_) {
      throw PlatformException(
        code: 'state_directory',
        message: 'Could not create a private Linux state directory.',
      );
    }
  }
  // The application ID survives versioned deployment paths and launcher changes.
  final application = sha256.convert(utf8.encode(applicationId)).toString();
  return p.posix.join(dataHome, application, 'sing_box_flutter');
}

final _gio = _Gio();

// GIO is already provided by Flutter's GTK host. Its attribute API avoids
// guessing platform-specific struct stat layouts for ownership checks.
class _Gio {
  final _library = ffi.DynamicLibrary.open('libgio-2.0.so.0');
  late final _defaultApplication = _library.lookupFunction<
      ffi.Pointer<ffi.Void> Function(),
      ffi.Pointer<ffi.Void> Function()>('g_application_get_default');
  late final _applicationId = _library.lookupFunction<
      ffi.Pointer<ffi.Uint8> Function(ffi.Pointer<ffi.Void>),
      ffi.Pointer<ffi.Uint8> Function(
          ffi.Pointer<ffi.Void>)>('g_application_get_application_id');
  late final _fileForPath = _library.lookupFunction<
      ffi.Pointer<ffi.Void> Function(ffi.Pointer<ffi.Uint8>),
      ffi.Pointer<ffi.Void> Function(
          ffi.Pointer<ffi.Uint8>)>('g_file_new_for_path');
  late final _queryInfo = _library.lookupFunction<
      ffi.Pointer<ffi.Void> Function(
          ffi.Pointer<ffi.Void>,
          ffi.Pointer<ffi.Uint8>,
          ffi.Int32,
          ffi.Pointer<ffi.Void>,
          ffi.Pointer<ffi.Pointer<ffi.Void>>),
      ffi.Pointer<ffi.Void> Function(
          ffi.Pointer<ffi.Void>,
          ffi.Pointer<ffi.Uint8>,
          int,
          ffi.Pointer<ffi.Void>,
          ffi.Pointer<ffi.Pointer<ffi.Void>>)>('g_file_query_info');
  late final _hasAttribute = _library.lookupFunction<
      ffi.Int32 Function(ffi.Pointer<ffi.Void>, ffi.Pointer<ffi.Uint8>),
      int Function(ffi.Pointer<ffi.Void>,
          ffi.Pointer<ffi.Uint8>)>('g_file_info_has_attribute');
  late final _uint32Attribute = _library.lookupFunction<
      ffi.Uint32 Function(ffi.Pointer<ffi.Void>, ffi.Pointer<ffi.Uint8>),
      int Function(ffi.Pointer<ffi.Void>,
          ffi.Pointer<ffi.Uint8>)>('g_file_info_get_attribute_uint32');
  late final _unref = _library.lookupFunction<
      ffi.Void Function(ffi.Pointer<ffi.Void>),
      void Function(ffi.Pointer<ffi.Void>)>('g_object_unref');

  String? applicationId() {
    final application = _defaultApplication();
    if (application == ffi.nullptr) return null;
    final id = _applicationId(application);
    if (id == ffi.nullptr) return null;
    // A valid GApplication ID is at most 255 ASCII bytes. Never scan unbounded
    // native memory when a custom host supplies an unavailable/invalid ID.
    final bytes = <int>[];
    for (var index = 0; index <= 255; index++) {
      final byte = id[index];
      if (byte == 0) return utf8.decode(bytes);
      bytes.add(byte);
    }
    return null;
  }

  ({int uid, int mode}) directoryMetadata(String path) =>
      _withCString(path, (nativePath) {
        final file = _fileForPath(nativePath);
        if (file == ffi.nullptr) {
          throw const FileSystemException('Could not inspect directory.');
        }
        try {
          return _withCString('unix::uid,unix::mode', (attributes) {
            // G_FILE_QUERY_INFO_NOFOLLOW_SYMLINKS = 1: inspect the final entry.
            final info =
                _queryInfo(file, attributes, 1, ffi.nullptr, ffi.nullptr);
            if (info == ffi.nullptr) {
              throw const FileSystemException('Could not inspect directory.');
            }
            try {
              int attribute(String name) => _withCString(name, (nativeName) {
                    if (_hasAttribute(info, nativeName) == 0) {
                      throw const FileSystemException(
                          'Directory ownership is unavailable.');
                    }
                    return _uint32Attribute(info, nativeName);
                  });
              return (
                uid: attribute('unix::uid'),
                mode: attribute('unix::mode')
              );
            } finally {
              _unref(info);
            }
          });
        } finally {
          _unref(file);
        }
      });
}

T _withCString<T>(String value, T Function(ffi.Pointer<ffi.Uint8>) action) {
  final encoded = utf8.encode(value);
  if (encoded.contains(0)) {
    throw const FormatException('Invalid native string.');
  }
  final libc = ffi.DynamicLibrary.process();
  final malloc = libc.lookupFunction<ffi.Pointer<ffi.Void> Function(ffi.IntPtr),
      ffi.Pointer<ffi.Void> Function(int)>('malloc');
  final free = libc.lookupFunction<ffi.Void Function(ffi.Pointer<ffi.Void>),
      void Function(ffi.Pointer<ffi.Void>)>('free');
  final buffer = malloc(encoded.length + 1).cast<ffi.Uint8>();
  if (buffer == ffi.nullptr) {
    throw const FileSystemException('Could not allocate native string.');
  }
  try {
    buffer.asTypedList(encoded.length + 1)
      ..setAll(0, encoded)
      ..[encoded.length] = 0;
    return action(buffer);
  } finally {
    free(buffer.cast<ffi.Void>());
  }
}

({int uid, int mode}) _directoryMetadata(String path) {
  if (Platform.isLinux) return _gio.directoryMetadata(path);
  // This Linux backend is also filesystem-tested on macOS. The system stat
  // utility provides no-follow metadata there without a guessed Darwin ABI.
  if (Platform.isMacOS) {
    final result = Process.runSync('/usr/bin/stat', ['-f', '%u %p', path]);
    if (result.exitCode == 0) {
      final fields = (result.stdout as String).trim().split(RegExp(r'\s+'));
      if (fields.length == 2) {
        return (
          uid: int.parse(fields[0]),
          mode: int.parse(fields[1], radix: 8)
        );
      }
    }
  }
  throw const FileSystemException('Directory ownership is unavailable.');
}

void _validateTemporaryParent(String parent) {
  final getuid = ffi.DynamicLibrary.process()
      .lookupFunction<ffi.Uint32 Function(), int Function()>('getuid');
  final uid = getuid();
  var ancestor = '/';
  for (final component in p.posix.split(parent)) {
    ancestor = p.posix.join(ancestor, component);
    final metadata = _directoryMetadata(ancestor);
    final mode = metadata.mode;
    if (mode & 0xf000 != 0x4000 || // S_IFMT / S_IFDIR
        (metadata.uid != uid && metadata.uid != 0) ||
        (mode & 0x12 != 0 && // group/other write (0022)
            !(metadata.uid == 0 && mode & 0x200 != 0))) {
      // sticky (01000)
      throw const FileSystemException('Unsafe temporary directory ancestry.');
    }
  }
}

String _privateTemporaryDirectory(
    String parent, void Function(String)? onCreated) {
  // Dart's Directory.createTemp does not guarantee 0700 on Linux. POSIX mkdtemp
  // creates the unpredictable directory atomically with private permissions,
  // without a process-wide umask change or a create-then-chmod exposure window.
  // Resolve only the parent, then check its entire ancestry before creating
  // anything. Never follow a replacement symlink at the newly created child.
  if (parent.contains('\u0000')) {
    throw const FormatException('Invalid temporary directory.');
  }
  final canonicalParent = Directory(parent).resolveSymbolicLinksSync();
  _validateTemporaryParent(canonicalParent);
  final template =
      utf8.encode(p.posix.join(canonicalParent, 'sing-box-flutter-XXXXXX'));
  if (template.contains(0)) {
    throw const FormatException('Invalid temporary directory.');
  }
  final libc = ffi.DynamicLibrary.process();
  final malloc = libc.lookupFunction<ffi.Pointer<ffi.Void> Function(ffi.IntPtr),
      ffi.Pointer<ffi.Void> Function(int)>('malloc');
  final free = libc.lookupFunction<ffi.Void Function(ffi.Pointer<ffi.Void>),
      void Function(ffi.Pointer<ffi.Void>)>('free');
  final mkdtemp = libc.lookupFunction<
      ffi.Pointer<ffi.Uint8> Function(ffi.Pointer<ffi.Uint8>),
      ffi.Pointer<ffi.Uint8> Function(ffi.Pointer<ffi.Uint8>)>('mkdtemp');
  final buffer = malloc(template.length + 1).cast<ffi.Uint8>();
  if (buffer == ffi.nullptr) {
    throw const FileSystemException('Could not allocate temporary path.');
  }
  try {
    final bytes = buffer.asTypedList(template.length + 1);
    bytes.setAll(0, template);
    bytes[template.length] = 0;
    if (mkdtemp(buffer) == ffi.nullptr) {
      throw const FileSystemException('Could not create temporary directory.');
    }
    // mkdtemp replaces the six ASCII Xs in place and preserves the byte length.
    final path = utf8.decode(bytes.sublist(0, template.length));
    onCreated?.call(path);
    return path;
  } finally {
    free(buffer.cast<ffi.Void>());
  }
}

/// Linux backend using a bundled, source-built sing-box child process.
///
/// The helper alone needs TUN capabilities. It cleans up on parent pipe closure;
/// subscriptions and widgets do not own its lifetime, but the Flutter engine does.
class LinuxSingBoxFlutter extends SingBoxFlutterPlatform {
  /// Creates the Linux platform implementation.
  LinuxSingBoxFlutter()
      : _bundleDirectory = p.join(
          p.dirname(Platform.resolvedExecutable),
          'lib',
          'sing_box_flutter',
        ),
        _dataDirectory = linuxDataDirectory(
          environment: Platform.environment,
          applicationId: linuxApplicationId(
            applicationIdOverride:
                const bool.hasEnvironment('SING_BOX_FLUTTER_APPLICATION_ID')
                    ? const String.fromEnvironment(
                        'SING_BOX_FLUTTER_APPLICATION_ID')
                    : null,
          ),
          temporaryDirectory: Directory.systemTemp.path,
        ),
        _startProcess = Process.start;

  /// Supplies isolated paths and a process factory for protocol tests.
  @visibleForTesting
  LinuxSingBoxFlutter.forTesting({
    required String bundleDirectory,
    required String dataDirectory,
    required Future<Process> Function(String, List<String>) startProcess,
  })  : _bundleDirectory = bundleDirectory,
        _dataDirectory = dataDirectory,
        _startProcess = startProcess;

  /// Called by Flutter's generated Dart plugin registrant.
  static void registerWith() {
    SingBoxFlutterPlatform.instance = LinuxSingBoxFlutter();
  }

  final String _bundleDirectory;
  final String _dataDirectory;
  final Future<Process> Function(String, List<String>) _startProcess;
  final _updates = StreamController<SingBoxEvent>.broadcast();
  final _pending = <int, Completer<Object?>>{};
  Process? _process;
  Future<void>? _launching;
  Future<void>? _stopping;
  Future<void> _writes = Future<void>.value();
  bool _initialized = false;
  int _nextId = 0;
  String? _version;
  VpnStatus _status = const VpnStatus(state: VpnConnectionState.disconnected);

  // The helper's scanner allows at most 16 MiB, including the trailing newline.
  static const _maxRequestBytes = 16 * 1024 * 1024;

  static PlatformException _error(String code, String message) =>
      PlatformException(code: code, message: message);

  void _updateStatus(VpnStatus status) {
    _status = status;
    _updates.add(status);
  }

  Future<void> _ensureProcess() => _launching ??= _launch().whenComplete(() {
        _launching = null;
      });

  Future<void> _launch() async {
    if (_process != null) return;
    Process? child;
    try {
      // Do not overlap a replacement core with one still removing its routes.
      await _stopping?.timeout(const Duration(seconds: 15), onTimeout: () {
        throw _error(
            'core_stopping', 'The previous Linux core is still stopping.');
      });
      final manifest = jsonDecode(await File(
        p.join(_bundleDirectory, 'core-capabilities.json'),
      ).readAsString()) as Map<String, dynamic>;
      final fingerprint = manifest['fingerprint'];
      if (fingerprint is! String ||
          !RegExp(r'^[a-f0-9]{64}$').hasMatch(fingerprint)) {
        throw const FormatException('Invalid core fingerprint.');
      }
      child = await _startProcess(
        p.join(_bundleDirectory, 'sing-box-core'),
        ['--data-dir', p.absolute(_dataDirectory)],
      );
      _process = child;
      final current = child;
      child.stdout
          .transform(utf8.decoder)
          .transform(const LineSplitter())
          .listen(
            (line) => _receive(current, line),
            onError: (Object _) => _failed(current),
            onDone: () => _failed(current),
          );
      // Raw upstream diagnostics may contain credentials. Never forward them.
      child.stderr.listen((_) {}, onError: (Object _) {});
      unawaited(child.exitCode.then((_) => _failed(current)));
      final info = await _send('getInfo') as Map;
      if (info['fingerprint'] != fingerprint ||
          info['version'] is! String ||
          (info['version'] as String).isEmpty) {
        throw _error('core_mismatch',
            'The bundled Linux core does not match its capabilities manifest.');
      }
      _version = info['version'] as String;
    } catch (error) {
      if (child != null) _failed(child);
      if (error is PlatformException) rethrow;
      throw _error('missing_native_core',
          'Build and bundle the Linux core with dart run sing_box_flutter build --target linux.');
    }
  }

  void _receive(Process child, String line) {
    if (!identical(_process, child)) return;
    try {
      final value = jsonDecode(line) as Map<String, dynamic>;
      if (value['type'] == 'status') {
        _updateStatus(VpnStatus.fromMap(value));
        return;
      }
      final completion = _pending.remove(value['id']);
      if (completion == null) throw const FormatException('Unknown response.');
      final failure = value['error'];
      if (failure is Map &&
          failure['code'] is String &&
          failure['message'] is String) {
        completion.completeError(
            _error(failure['code'] as String, failure['message'] as String));
      } else if (value.containsKey('result')) {
        completion.complete(value['result']);
      } else {
        completion.completeError(
            _error('core_protocol', 'Malformed Linux core response.'));
        _failed(child);
      }
    } catch (_) {
      _failed(child);
    }
  }

  void _failed(Process child) {
    if (!identical(_process, child)) return;
    _process = null;
    _stopping = child.exitCode.then((_) {});
    _initialized = false;
    _version = null;
    // Closing stdin and SIGTERM both trigger orderly TUN/route cleanup.
    try {
      unawaited(child.stdin.close().catchError((Object _) {}));
    } catch (_) {
      // IOSink.close can throw synchronously while a flush is in progress.
    }
    child.kill(ProcessSignal.sigterm);
    final error = _error('core_exited',
        'The Linux core exited. Initialize again before reconnecting.');
    for (final completion in _pending.values) {
      completion.completeError(error);
    }
    _pending.clear();
    _updateStatus(const VpnStatus(
      state: VpnConnectionState.error,
      message: 'The Linux core exited.',
    ));
  }

  Future<Object?> _send(String method, [String? config]) async {
    final child = _process;
    if (child == null) {
      throw _error('core_exited', 'The Linux core is not running.');
    }
    final id = _nextId + 1;
    final payload = JsonUtf8Encoder().convert({
      'id': id,
      'method': method,
      if (config != null) 'config': config,
    });
    // Reject locally before creating pending requests, timers or pipe writes.
    // Leave one byte for the newline consumed by the native scanner.
    if (payload.length >= _maxRequestBytes) {
      throw _error('request_too_large',
          'The Linux core request exceeds the 16 MiB IPC limit.');
    }
    _nextId = id;
    final completion = Completer<Object?>();
    _pending[id] = completion;
    // Attach the error/timeout handler before writing to a possibly broken pipe.
    final response =
        completion.future.timeout(const Duration(minutes: 2), onTimeout: () {
      _failed(child);
      throw _error('core_timeout', 'The Linux core did not respond.');
    });
    // IOSink cannot accept writes while another flush is bound to its stream.
    // Serialize the pipe writes while retaining independent response futures.
    _writes = _writes.then((_) async {
      if (!identical(_process, child)) return;
      child.stdin.add(payload);
      child.stdin.add(const [0x0a]);
      await child.stdin.flush();
    }).catchError((Object _) {
      _failed(child);
    });
    return response;
  }

  Future<Object?> _call(String method, [String? config]) {
    if (!_initialized) {
      throw _error('not_initialized', 'Call initialize before using the VPN.');
    }
    return _send(method, config);
  }

  @override
  Future<void> initialize({
    String? appGroup,
    String? providerBundleIdentifier,
    required String notificationTitle,
  }) async {
    await _ensureProcess();
    await _send('initialize');
    _updateStatus(VpnStatus.fromMap(await _send('getStatus') as Map));
    _initialized = true;
  }

  @override
  Future<String> getVersion() async {
    await _ensureProcess();
    return _version!;
  }

  @override
  Future<void> checkConfig(String config) async {
    await _call('checkConfig', config);
  }

  /// Checks TUN access; Linux has no mobile-style VPN consent dialog.
  @override
  Future<bool> requestPermission() async =>
      await _call('requestPermission') as bool;

  @override
  Future<void> start({required String config, required String name}) async {
    await _call('start', config);
  }

  @override
  Future<void> stop() async {
    await _call('stop');
  }

  @override
  Future<VpnStatus> getStatus() async =>
      VpnStatus.fromMap(await _call('getStatus') as Map);

  @override
  Stream<SingBoxEvent> get events => Stream<SingBoxEvent>.multi((controller) {
        final subscription = _updates.stream.listen(
          controller.add,
          onError: controller.addError,
        );
        controller.add(_status);
        controller.onCancel = subscription.cancel;
      }, isBroadcast: true);
}
