import 'dart:async';
import 'dart:convert';
import 'dart:ffi' as ffi;
import 'dart:io';

import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:path/path.dart' as p;
import 'package:sing_box_flutter/sing_box_flutter.dart';
import 'package:sing_box_flutter/sing_box_flutter_linux.dart'
    show linuxApplicationId, linuxDataDirectory;

const fingerprint =
    '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';
const maxRequestBytes = 16 * 1024 * 1024;

String configWithRequestBytes(int id, int size, {String character = 'a'}) {
  String config(String padding) => jsonEncode({
        'outbounds': [
          {'type': 'direct', 'tag': 'secret-auth-key$padding'},
        ],
      });
  final overhead = utf8
      .encode(jsonEncode({
        'id': id,
        'method': 'checkConfig',
        'config': config(''),
      }))
      .length;
  final paddingBytes = size - overhead;
  final characterBytes = utf8.encode(character).length;
  return config(
    character * (paddingBytes ~/ characterBytes) +
        'a' * (paddingBytes % characterBytes),
  );
}

class FakeCoreProcess implements Process {
  FakeCoreProcess() {
    _input.stream
        .transform(utf8.decoder)
        .transform(const LineSplitter())
        .listen(
      (line) {
        final request = jsonDecode(line) as Map<String, dynamic>;
        requests.add(request);
        if (holdRequests) return;
        Object? result;
        switch (request['method']) {
          case 'getInfo':
            result = {
              'version': '1.14.4',
              'fingerprint': coreFingerprint,
              'process_id': '42',
              'process_start_time': processStartTime,
            };
          case 'requestPermission':
            result = permission;
          case 'getStatus':
            result = {'type': 'status', 'state': state};
          case 'start':
            emit({'type': 'status', 'state': 'connecting'});
            state = 'connected';
            emit({'type': 'status', 'state': state});
          case 'stop':
            if (failCleanup) {
              if (cleanupStatusFirst) {
                emit({
                  'type': 'status',
                  'state': 'error',
                  'code': 'cleanup_failed',
                  'message': 'Replace the helper before reconnecting.',
                });
              }
              emit({
                'id': request['id'],
                'error': {
                  'code': 'cleanup_failed',
                  'message': 'Replace the helper before reconnecting.',
                },
              });
              return;
            }
            state = 'disconnected';
            emit({'type': 'status', 'state': state});
          case 'checkConfig':
            if (rejectConfig) {
              emit({
                'id': request['id'],
                'error': {
                  'code': 'invalid_config',
                  'message': 'The Linux core rejected the configuration.',
                },
              });
              return;
            }
        }
        emit({'id': request['id'], 'result': result});
      },
    );
    stdin = IOSink(_input.sink);
  }

  final _input = StreamController<List<int>>();
  final _output = StreamController<List<int>>();
  final _errors = StreamController<List<int>>();
  final _exit = Completer<int>();
  final requests = <Map<String, dynamic>>[];
  String coreFingerprint = fingerprint;
  String processStartTime = '100';
  String state = 'disconnected';
  bool permission = true;
  bool rejectConfig = false;
  bool failCleanup = false;
  bool cleanupStatusFirst = false;
  bool holdRequests = false;
  int kills = 0;

  void emit(Map<String, Object?> value) =>
      _output.add(utf8.encode('${jsonEncode(value)}\n'));

  void exit() {
    if (_exit.isCompleted) return;
    _exit.complete(1);
    unawaited(_output.close());
    unawaited(_errors.close());
  }

  @override
  late final IOSink stdin;
  @override
  Stream<List<int>> get stdout => _output.stream;
  @override
  Stream<List<int>> get stderr => _errors.stream;
  @override
  Future<int> get exitCode => _exit.future;
  @override
  int get pid => 42;
  @override
  bool kill([ProcessSignal signal = ProcessSignal.sigterm]) {
    kills++;
    exit();
    return true;
  }
}

void main() {
  late Directory bundle;
  late FakeCoreProcess child;
  late LinuxSingBoxFlutter plugin;
  late List<String> arguments;
  late String executable;

  setUp(() async {
    bundle = await Directory.systemTemp.createTemp('sing-box-linux-test-');
    await File('${bundle.path}/core-capabilities.json')
        .writeAsString(jsonEncode({'fingerprint': fingerprint}));
    child = FakeCoreProcess();
    plugin = LinuxSingBoxFlutter.forTesting(
      bundleDirectory: bundle.path,
      dataDirectory: p.join(bundle.path, 'state'),
      startProcess: (path, args) async {
        executable = path;
        arguments = args;
        return child;
      },
    );
  });

  tearDown(() async {
    child.exit();
    await Future<void>.delayed(Duration.zero);
    await bundle.delete(recursive: true);
  });

  test('an absolute XDG data home takes precedence over HOME', () {
    final directory = linuxDataDirectory(
      environment: {'XDG_DATA_HOME': '/data/app', 'HOME': 'relative/home'},
      applicationId: 'dev.test.Vpn',
      temporaryDirectory: '/tmp',
    );
    expect(directory, matches(r'^/data/app/[a-f0-9]{64}/sing_box_flutter$'));
  });

  test('empty and relative XDG data homes use the XDG default', () {
    for (final xdgDataHome in [
      null,
      '',
      'data',
      './data',
      '../data',
      '~/data'
    ]) {
      expect(
        linuxDataDirectory(
          environment: {
            if (xdgDataHome != null) 'XDG_DATA_HOME': xdgDataHome,
            'HOME': '/home/alice',
          },
          applicationId: 'dev.test.Vpn',
          temporaryDirectory: '/tmp',
        ),
        matches(r'^/home/alice/\.local/share/[a-f0-9]{64}/sing_box_flutter$'),
        reason: 'XDG_DATA_HOME=$xdgDataHome',
      );
    }
  });

  test('application identity survives versioned launcher upgrades', () async {
    final first = File('${bundle.path}/vendor-a/1.0/vpn');
    final upgraded = File('${bundle.path}/vendor-a/2.0/vpn');
    final other = File('${bundle.path}/vendor-b/1.0/vpn');
    for (final file in [first, upgraded, other]) {
      await file.parent.create(recursive: true);
      await file.writeAsString('executable fixture');
    }
    final launcher =
        await Link('${bundle.path}/vpn-launcher').create(first.path);
    String directory(String applicationId) => linuxDataDirectory(
          environment: {'XDG_DATA_HOME': '${bundle.path}/user-data'},
          applicationId: applicationId,
          temporaryDirectory: bundle.path,
        );
    final before = await launcher.resolveSymbolicLinks();
    final firstDirectory = directory('dev.vendor_a.Vpn');
    await launcher.update(upgraded.path);
    expect(await launcher.resolveSymbolicLinks(), isNot(before));
    expect(firstDirectory, directory('dev.vendor_a.Vpn'));
    expect(firstDirectory, isNot(directory('dev.vendor_b.Vpn')));
    expect(firstDirectory, startsWith('${bundle.path}/user-data/'));
    expect(await Directory('${bundle.path}/user-data').exists(), isFalse);
  }, skip: Platform.isWindows);

  test('application override takes precedence over GTK identity', () {
    expect(
      linuxApplicationId(
        applicationIdOverride: 'dev.custom.Host',
        readApplicationId: () => throw StateError('must not read GTK'),
      ),
      'dev.custom.Host',
    );
    expect(linuxApplicationId(readApplicationId: () => 'dev.gtk.Host'),
        'dev.gtk.Host');
  });

  test('missing and invalid application identities fail sanitized', () {
    for (final id in [
      null,
      '',
      'vpn',
      '/opt/vendor/vpn',
      'dev..Vpn',
      'dev.1Vpn',
      'dev.Vpn\n',
      'dev.私密',
      'dev.${'a' * 252}',
    ]) {
      expect(
        () => linuxApplicationId(readApplicationId: () => id),
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'application_identity')
            .having((error) => error.toString(), 'message',
                isNot(contains('/opt/vendor/vpn')))),
        reason: 'ID=$id',
      );
    }
    expect(
      () => linuxApplicationId(
        applicationIdOverride: 'private-path-secret',
        readApplicationId: () => 'dev.valid.Host',
      ),
      throwsA(isA<PlatformException>()
          .having((error) => error.code, 'code', 'application_identity')
          .having((error) => error.toString(), 'message',
              isNot(contains('private-path-secret')))),
    );
    expect(
      () => linuxApplicationId(
          readApplicationId: () => throw StateError('private-path-secret')),
      throwsA(isA<PlatformException>().having((error) => error.toString(),
          'message', isNot(contains('private-path-secret')))),
    );
  });

  test('GTK default application supplies the stable native identity', () {
    final gio = ffi.DynamicLibrary.open('libgio-2.0.so.0');
    final getDefault = gio.lookupFunction<ffi.Pointer<ffi.Void> Function(),
        ffi.Pointer<ffi.Void> Function()>('g_application_get_default');
    final setDefault = gio.lookupFunction<
        ffi.Void Function(ffi.Pointer<ffi.Void>),
        void Function(ffi.Pointer<ffi.Void>)>('g_application_set_default');
    final create = gio.lookupFunction<
        ffi.Pointer<ffi.Void> Function(ffi.Pointer<ffi.Uint8>, ffi.Int32),
        ffi.Pointer<ffi.Void> Function(
            ffi.Pointer<ffi.Uint8>, int)>('g_application_new');
    final unref = gio.lookupFunction<ffi.Void Function(ffi.Pointer<ffi.Void>),
        void Function(ffi.Pointer<ffi.Void>)>('g_object_unref');
    final libc = ffi.DynamicLibrary.process();
    final malloc = libc.lookupFunction<
        ffi.Pointer<ffi.Void> Function(ffi.IntPtr),
        ffi.Pointer<ffi.Void> Function(int)>('malloc');
    final free = libc.lookupFunction<ffi.Void Function(ffi.Pointer<ffi.Void>),
        void Function(ffi.Pointer<ffi.Void>)>('free');
    final bytes = utf8.encode('dev.test.GtkVpn');
    final id = malloc(bytes.length + 1).cast<ffi.Uint8>();
    expect(id, isNot(ffi.nullptr));
    id.asTypedList(bytes.length + 1)
      ..setAll(0, bytes)
      ..[bytes.length] = 0;
    final previous = getDefault();
    final application = create(id, 0);
    expect(application, isNot(ffi.nullptr));
    try {
      setDefault(application);
      expect(linuxApplicationId(), 'dev.test.GtkVpn');
      setDefault(ffi.nullptr);
      expect(
        linuxApplicationId,
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'application_identity')),
      );
    } finally {
      setDefault(previous);
      unref(application);
      free(id.cast<ffi.Void>());
    }
  }, skip: !Platform.isLinux);

  test('invalid HOME creates a fresh private temporary fallback', () async {
    final directories = <String>{};
    for (final home in [null, '', 'alice', './alice', '../alice', '~']) {
      final directory = linuxDataDirectory(
        environment: {
          'XDG_DATA_HOME': 'relative/data',
          if (home != null) 'HOME': home,
          'UID': '0',
          'USER': 'another-user',
        },
        applicationId: 'dev.test.Vpn',
        temporaryDirectory: bundle.path,
      );
      expect(
          directory,
          startsWith(
              '${await bundle.resolveSymbolicLinks()}/sing-box-flutter-'));
      expect(directories.add(directory), isTrue, reason: 'HOME=$home');
      expect(await Directory(directory).exists(), isTrue);
      if (!Platform.isWindows) {
        final mode = (await Directory(directory).stat()).mode;
        expect(mode & 0x3f, 0);
        expect(mode & 0x1ff, 0x1c0); // 0700, including owner access.
      }
    }
    expect(
      () => linuxDataDirectory(
        environment: {},
        applicationId: 'dev.test.Vpn',
        temporaryDirectory: 'relative/tmp',
      ),
      throwsArgumentError,
    );
  }, skip: Platform.isWindows);

  test('private temporary fallback preserves UTF-8 directory paths', () async {
    final parent = await Directory('${bundle.path}/tmp-空间-\u{10400}').create();
    final directory = linuxDataDirectory(
      environment: {},
      applicationId: 'dev.test.Vpn',
      temporaryDirectory: parent.path,
    );
    expect(directory,
        startsWith('${await parent.resolveSymbolicLinks()}/sing-box-flutter-'));
    expect(await Directory(directory).exists(), isTrue);
    expect((await Directory(directory).stat()).mode & 0x1ff, 0x1c0);
  }, skip: Platform.isWindows);

  test('fallback rejects nonsticky writable temporary ancestry before creation',
      () async {
    for (final mode in ['0777', '0770']) {
      final unsafe = await Directory('${bundle.path}/unsafe-$mode').create();
      final chmod = Process.runSync('/bin/chmod', [mode, unsafe.path]);
      expect(chmod.exitCode, 0, reason: chmod.stderr.toString());
      for (final nested in [false, true]) {
        final parent =
            nested ? await Directory('${unsafe.path}/nested').create() : unsafe;
        final before = await parent.list().length;
        expect(
          () => linuxDataDirectory(
            environment: {},
            applicationId: 'dev.test.Vpn',
            temporaryDirectory: parent.path,
          ),
          throwsA(isA<PlatformException>()
              .having((error) => error.code, 'code', 'state_directory')),
        );
        expect(await parent.list().length, before);
      }
    }
  }, skip: Platform.isWindows);

  test('fallback supports a safe symlinked temporary parent', () async {
    final target = await Directory('${bundle.path}/temporary-parent').create();
    final alias =
        await Link('${bundle.path}/temporary-alias').create(target.path);
    final directory = linuxDataDirectory(
      environment: {},
      applicationId: 'dev.test.Vpn',
      temporaryDirectory: alias.path,
    );
    expect(directory,
        startsWith('${await target.resolveSymbolicLinks()}/sing-box-flutter-'));
    expect(directory, isNot(startsWith('${alias.path}/')));
    expect((await Directory(directory).stat()).mode & 0x1ff, 0x1c0);
  }, skip: Platform.isWindows);

  test('fallback works under the real system temporary directory', () async {
    final directory = linuxDataDirectory(
      environment: {},
      applicationId: 'dev.test.Vpn',
      temporaryDirectory: Directory.systemTemp.path,
    );
    addTearDown(() => Directory(directory).delete(recursive: true));
    expect((await Directory(directory).stat()).mode & 0x1ff, 0x1c0);
  }, skip: Platform.isWindows);

  test('fallback never follows a replaced child after mkdtemp', () async {
    final target =
        await Directory('${bundle.path}/replacement-target').create();
    final marker =
        await File('${target.path}/marker').writeAsString('untouched');
    late String created;
    final directory = linuxDataDirectory(
      environment: {},
      applicationId: 'dev.test.Vpn',
      temporaryDirectory: bundle.path,
      onTemporaryDirectoryCreated: (path) {
        created = path;
        Directory(path).renameSync('$path-original');
        Link(path).createSync(target.path);
      },
    );
    expect(directory, created);
    expect(directory, isNot(await target.resolveSymbolicLinks()));
    expect(await FileSystemEntity.type(directory, followLinks: false),
        FileSystemEntityType.link);
    expect(await marker.readAsString(), 'untouched');
  }, skip: Platform.isWindows);

  test('temporary fallback rejects NUL paths before native path truncation',
      () async {
    final entries = await bundle.list().length;
    expect(
      () => linuxDataDirectory(
        environment: {},
        applicationId: 'dev.test.Vpn',
        temporaryDirectory: '${bundle.path}\u0000/private-path-secret',
      ),
      throwsA(isA<PlatformException>()
          .having((error) => error.code, 'code', 'state_directory')
          .having((error) => error.toString(), 'message',
              isNot(contains('private-path-secret')))),
    );
    expect(await bundle.list().length, entries);
  }, skip: Platform.isWindows);

  test('fallback ignores precreated predictable paths and symlinks', () async {
    final target = await Directory('${bundle.path}/other-user').create();
    final marker =
        await File('${target.path}/marker').writeAsString('unchanged');
    final predictable = Directory('${bundle.path}/.local/share/my_app');
    await predictable.create(recursive: true);
    await Link('${predictable.path}/sing_box_flutter').create(target.path);
    final directory = linuxDataDirectory(
      environment: {'HOME': '', 'XDG_DATA_HOME': ''},
      applicationId: 'dev.test.Vpn',
      temporaryDirectory: bundle.path,
    );
    expect(directory, isNot('${predictable.path}/sing_box_flutter'));
    expect(directory, startsWith('${await bundle.resolveSymbolicLinks()}/'));
    expect(await Directory(directory).list().isEmpty, isTrue);
    expect(await marker.readAsString(), 'unchanged');
    expect(await target.list().length, 1);
  }, skip: Platform.isWindows);

  test('temporary fallback failures report sanitized errors', () async {
    final invalidTemp = await File('${bundle.path}/private-path-secret')
        .writeAsString('not a directory');
    expect(
      () => linuxDataDirectory(
        environment: {},
        applicationId: 'dev.test.Vpn',
        temporaryDirectory: invalidTemp.path,
      ),
      throwsA(isA<PlatformException>()
          .having((error) => error.code, 'code', 'state_directory')
          .having((error) => error.toString(), 'message',
              isNot(contains('private-path-secret')))),
    );
  }, skip: Platform.isWindows);

  test('fallback allocation is lazy and a replacement gets fresh owned state',
      () async {
    var allocations = 0;
    final launches = <List<String>>[];
    plugin = LinuxSingBoxFlutter.forTesting(
      bundleDirectory: bundle.path,
      dataDirectory: 'unused',
      temporaryState: true,
      createDataDirectory: () =>
          p.join(bundle.path, 'private-${++allocations}'),
      startProcess: (_, args) async {
        launches.add(args);
        return child;
      },
    );
    expect(allocations, 0);
    await plugin.initialize(notificationTitle: 'Test');
    await plugin.getVersion();
    expect(allocations, 1);
    final failure = plugin.events.firstWhere(
      (event) => event is VpnStatus && event.state == VpnConnectionState.error,
    );
    child.exit();
    await failure;
    child = FakeCoreProcess();
    await plugin.initialize(notificationTitle: 'Test');
    expect(allocations, 2);
    expect(launches, [
      ['--data-dir', p.join(bundle.path, 'private-1'), '--temporary-state'],
      ['--data-dir', p.join(bundle.path, 'private-2'), '--temporary-state'],
    ]);
  });

  test('fallback allocation waits until the manifest is valid', () async {
    var allocated = false;
    plugin = LinuxSingBoxFlutter.forTesting(
      bundleDirectory: bundle.path,
      dataDirectory: 'unused',
      temporaryState: true,
      createDataDirectory: () {
        allocated = true;
        return p.join(bundle.path, 'private');
      },
      startProcess: (_, __) async => child,
    );
    await File(p.join(bundle.path, 'core-capabilities.json')).delete();
    await expectLater(plugin.getVersion(), throwsA(isA<PlatformException>()));
    expect(allocated, isFalse);
  });

  test('failed helper launch removes its unused temporary state', () async {
    late String directory;
    plugin = LinuxSingBoxFlutter.forTesting(
      bundleDirectory: bundle.path,
      dataDirectory: 'unused',
      temporaryState: true,
      createDataDirectory: () => directory = linuxDataDirectory(
        environment: {},
        applicationId: 'dev.test.Vpn',
        temporaryDirectory: bundle.path,
      ),
      startProcess: (_, __) async => throw const ProcessException('helper', []),
    );
    await expectLater(plugin.getVersion(), throwsA(isA<PlatformException>()));
    expect(await Directory(directory).exists(), isFalse);
  }, skip: Platform.isWindows);

  test('handshake verifies the bundled core before initialization', () async {
    expect(await plugin.getVersion(), '1.14.4');
    expect(child.requests.map((r) => r['method']), ['getInfo']);
    expect(() => plugin.stop(), throwsA(isA<PlatformException>()));
    await plugin.initialize(notificationTitle: 'Test');
    expect(executable, p.join(bundle.path, 'sing-box-core'));
    expect(arguments, ['--data-dir', p.join(bundle.path, 'state')]);
    expect(await plugin.requestPermission(), isTrue);
  });

  test('mismatched helper fails closed', () async {
    child.coreFingerprint = 'wrong';
    await expectLater(
      plugin.getVersion(),
      throwsA(isA<PlatformException>()
          .having((e) => e.code, 'code', 'core_mismatch')),
    );
    expect(child.kills, 1);
    expect(child.requests.any((r) => r['method'] == 'initialize'), isFalse);
  });

  test('configuration stays in the pipe and failures stay sanitized', () async {
    await plugin.initialize(notificationTitle: 'Test');
    const config = '{"password":"secret-auth-key"}';
    child.rejectConfig = true;
    await expectLater(
      plugin.checkConfig(config),
      throwsA(isA<PlatformException>().having(
        (e) => e.toString().contains('secret-auth-key'),
        'leaks credentials',
        isFalse,
      )),
    );
    expect(child.requests.last['config'], config);
    expect(arguments.join(' '), isNot(contains('secret-auth-key')));
    expect(await bundle.list().length, 1); // Only the capabilities fixture.
  });

  test('new subscribers see current status and cancellation does not stop',
      () async {
    await plugin.initialize(notificationTitle: 'Test');
    final states = <VpnConnectionState>[];
    final subscription = plugin.events.listen((event) {
      if (event is VpnStatus) states.add(event.state);
    });
    await plugin.start(config: '{}', name: 'Test');
    await Future<void>.delayed(Duration.zero);
    expect(states, [
      VpnConnectionState.disconnected,
      VpnConnectionState.connecting,
      VpnConnectionState.connected,
    ]);
    await subscription.cancel();
    expect(child.kills, 0);
    expect((await plugin.getStatus()).isConnected, isTrue);
    expect((await plugin.events.first as VpnStatus).isConnected, isTrue);
    await plugin.stop();
    expect((await plugin.getStatus()).state, VpnConnectionState.disconnected);
  });

  test('replacement initialization broadcasts its actual status after a crash',
      () async {
    await plugin.initialize(notificationTitle: 'Test');
    await plugin.start(config: '{}', name: 'Test');
    final states = <VpnConnectionState>[];
    final subscription = plugin.events.listen((event) {
      if (event is VpnStatus) states.add(event.state);
    });
    final failure = plugin.events.firstWhere(
      (event) => event is VpnStatus && event.state == VpnConnectionState.error,
    );
    child.exit();
    await failure;
    expect(states.last, VpnConnectionState.error);

    child = FakeCoreProcess();
    await plugin.initialize(notificationTitle: 'Test');
    await Future<void>.delayed(Duration.zero);
    expect(states.last, VpnConnectionState.disconnected);
    final replay = await plugin.events.first as VpnStatus;
    expect(replay.state, VpnConnectionState.disconnected);
    expect(replay.message, isNull);
    expect((await plugin.getStatus()).state, replay.state);
    expect(child.requests.map((request) => request['method']),
        ['getInfo', 'initialize', 'getStatus', 'getStatus']);
    await subscription.cancel();
  });

  test('reinitializing a connected helper preserves its native connection',
      () async {
    await plugin.initialize(notificationTitle: 'Test');
    await plugin.start(config: '{}', name: 'Test');
    final states = <VpnConnectionState>[];
    final subscription = plugin.events.listen((event) {
      if (event is VpnStatus) states.add(event.state);
    });
    await plugin.initialize(notificationTitle: 'Test');
    await Future<void>.delayed(Duration.zero);
    expect(states, isNotEmpty);
    expect(states, everyElement(VpnConnectionState.connected));
    expect((await plugin.events.first as VpnStatus).isConnected, isTrue);
    expect((await plugin.getStatus()).isConnected, isTrue);
    expect(child.requests.where((request) => request['method'] == 'start'),
        hasLength(1));
    expect(
        child.requests.any((request) => request['method'] == 'stop'), isFalse);
    expect(child.kills, 0);
    await subscription.cancel();
  });

  test('permission probe can return false without starting a connection',
      () async {
    child.permission = false;
    await plugin.initialize(notificationTitle: 'Test');
    expect(await plugin.requestPermission(), isFalse);
    expect(child.requests.any((r) => r['method'] == 'start'), isFalse);
  });

  test('concurrent API calls serialize pipe writes without killing the core',
      () async {
    await plugin.initialize(notificationTitle: 'Test');
    await Future.wait([
      plugin.getStatus(),
      plugin.checkConfig('{}'),
      plugin.requestPermission(),
      plugin.start(config: '{}', name: 'Test'),
      plugin.stop(),
    ]);
    expect(child.kills, 0);
    expect((await plugin.getStatus()).state, VpnConnectionState.disconnected);
  });

  test('IPC accepts a payload with exactly one byte left for its newline',
      () async {
    await plugin.initialize(notificationTitle: 'Test');
    final id = (child.requests.last['id'] as int) + 1;
    final config = configWithRequestBytes(id, maxRequestBytes - 1);
    await plugin.checkConfig(config);
    expect(utf8.encode(jsonEncode(child.requests.last)).length,
        maxRequestBytes - 1);
    expect(child.requests.last['config'], config);
    expect(child.kills, 0);
  });

  for (final character in ['a', '\u{10400}']) {
    test(
        'oversized UTF-8 IPC (${character == 'a' ? 'ASCII' : 'Unicode'}) '
        'leaves a connected VPN untouched', () async {
      await plugin.initialize(notificationTitle: 'Test');
      await plugin.start(config: '{}', name: 'Test');
      final lastId = child.requests.last['id'] as int;
      final requestCount = child.requests.length;
      final config = configWithRequestBytes(lastId + 1, maxRequestBytes,
          character: character);
      expect(() => jsonDecode(config), returnsNormally);
      expect(config.length, lessThan(maxRequestBytes));
      expect(
        utf8
            .encode(jsonEncode({
              'id': lastId + 1,
              'method': 'checkConfig',
              'config': config,
            }))
            .length,
        maxRequestBytes,
      );
      await expectLater(
        plugin.checkConfig(config),
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'request_too_large')
            .having((error) => error.toString().contains('secret-auth-key'),
                'leaks credentials', isFalse)),
      );
      await Future<void>.delayed(Duration.zero);
      expect(child.requests, hasLength(requestCount));
      expect(child.kills, 0);
      expect((await plugin.events.first as VpnStatus).isConnected, isTrue);
      expect((await plugin.getStatus()).isConnected, isTrue);
      expect(child.requests.last['id'], lastId + 1);
      await plugin.checkConfig('{}');
      expect(child.requests.last['method'], 'checkConfig');
      expect(child.kills, 0);
    });
  }

  for (final statusFirst in [false, true]) {
    test(
        'cleanup failure retains its code and retires the helper '
        '(status first: $statusFirst)', () async {
      await plugin.initialize(notificationTitle: 'Test');
      child.failCleanup = true;
      child.cleanupStatusFirst = statusFirst;
      await expectLater(
        plugin.stop(),
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'cleanup_failed')),
      );
      expect(child.kills, 1);
      final status = await plugin.events.first as VpnStatus;
      expect(status.state, VpnConnectionState.error);
      expect(status.message, contains('Could not fully close'));
      await expectLater(plugin.start(config: '{}', name: 'Test'),
          throwsA(isA<PlatformException>()));
      child = FakeCoreProcess();
      await plugin.initialize(notificationTitle: 'Test');
      await plugin.start(config: '{}', name: 'Test');
      expect((await plugin.getStatus()).isConnected, isTrue);
    });
  }

  test('cleanup failure status preserves the code for every pending request',
      () async {
    await plugin.initialize(notificationTitle: 'Test');
    child.holdRequests = true;
    final pending = [plugin.checkConfig('{}'), plugin.getStatus()];
    final assertions = pending
        .map((request) => expectLater(
              request,
              throwsA(isA<PlatformException>()
                  .having((error) => error.code, 'code', 'cleanup_failed')),
            ))
        .toList();
    child.emit({'type': 'status', 'state': 'error', 'code': 'cleanup_failed'});
    await Future.wait(assertions);
    expect(child.kills, 1);
  });

  test('late cleanup failure status retires a helper with no pending RPC',
      () async {
    await plugin.initialize(notificationTitle: 'Test');
    final failure = plugin.events.firstWhere((event) =>
        event is VpnStatus && event.state == VpnConnectionState.error);
    child.emit({
      'type': 'status',
      'state': 'error',
      'code': 'cleanup_failed',
      'message': 'Replace the helper before reconnecting.',
    });
    await failure;
    expect(child.kills, 1);
    await expectLater(plugin.getStatus(), throwsA(isA<PlatformException>()));
  });

  test('replacement waits for actual worker exit after supervisor death',
      () async {
    final workerExited = Completer<void>();
    addTearDown(() {
      if (!workerExited.isCompleted) workerExited.complete();
    });
    var launches = 0;
    var allocations = 0;
    plugin = LinuxSingBoxFlutter.forTesting(
      bundleDirectory: bundle.path,
      dataDirectory: 'unused',
      temporaryState: true,
      createDataDirectory: () =>
          p.join(bundle.path, 'private-${++allocations}'),
      stoppingTimeout: const Duration(milliseconds: 20),
      waitForWorkerExit: (pid, start) {
        expect(pid, 42);
        expect(start, '100');
        return workerExited.future;
      },
      startProcess: (_, __) async {
        launches++;
        return child;
      },
    );
    await plugin.initialize(notificationTitle: 'Test');
    final failed = plugin.events.firstWhere((event) =>
        event is VpnStatus && event.state == VpnConnectionState.error);
    child
        .exit(); // Supervisor gone; worker can still be in uninterruptible I/O.
    await failed;
    child = FakeCoreProcess();
    await expectLater(
        plugin.initialize(notificationTitle: 'Test'),
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'core_stopping')));
    expect(launches, 1);
    expect(allocations, 1);
    workerExited.complete();
    await plugin.initialize(notificationTitle: 'Test');
    expect(launches, 2);
    expect(allocations, 2);
    await plugin.start(config: '{}', name: 'Test');
    expect((await plugin.getStatus()).isConnected, isTrue);
  });

  test('invalid worker identity fails before any native initialization',
      () async {
    child.processStartTime = 'private-invalid-token';
    await expectLater(
        plugin.getVersion(),
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'core_identity')
            .having((error) => error.toString(), 'message',
                isNot(contains('private-invalid-token')))));
    expect(child.requests.map((request) => request['method']), ['getInfo']);
    expect(child.kills, 1);
  });

  test('core exit rejects in-flight requests and requires reinitialization',
      () async {
    await plugin.initialize(notificationTitle: 'Test');
    child.holdRequests = true;
    final pending = plugin.checkConfig('{}');
    final assertion = expectLater(
      pending,
      throwsA(isA<PlatformException>()
          .having((e) => e.code, 'code', 'core_exited')),
    );
    child.exit();
    await assertion;
    await expectLater(plugin.getStatus(), throwsA(isA<PlatformException>()));
    expect((await plugin.events.first as VpnStatus).state,
        VpnConnectionState.error);
  });
}
