import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter/sing_box_flutter.dart';
import 'package:sing_box_flutter/sing_box_flutter_linux.dart'
    show linuxDataDirectory;

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
            result = {'version': '1.14.4', 'fingerprint': coreFingerprint};
          case 'requestPermission':
            result = permission;
          case 'getStatus':
            result = {'type': 'status', 'state': state};
          case 'start':
            emit({'type': 'status', 'state': 'connecting'});
            state = 'connected';
            emit({'type': 'status', 'state': state});
          case 'stop':
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
  String state = 'disconnected';
  bool permission = true;
  bool rejectConfig = false;
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
      dataDirectory: '${bundle.path}/state',
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
      executable: '/opt/my_app/bin/my_app',
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
          executable: '/opt/my_app/bin/my_app',
          temporaryDirectory: '/tmp',
        ),
        matches(r'^/home/alice/\.local/share/[a-f0-9]{64}/sing_box_flutter$'),
        reason: 'XDG_DATA_HOME=$xdgDataHome',
      );
    }
  });

  test('canonical installation paths separate equal executable names',
      () async {
    final first = File('${bundle.path}/vendor-a/vpn');
    final second = File('${bundle.path}/vendor-b/vpn');
    for (final file in [first, second]) {
      await file.parent.create(recursive: true);
      await file.writeAsString('executable fixture');
    }
    final alias = await Link('${bundle.path}/vpn-alias').create(first.path);
    final dataHome = Directory('${bundle.path}/user-data');
    await dataHome.create();
    String directory(String executable) => linuxDataDirectory(
          environment: {'XDG_DATA_HOME': dataHome.path},
          executable: executable,
          temporaryDirectory: bundle.path,
        );
    final firstDirectory = directory(await first.resolveSymbolicLinks());
    expect(
        firstDirectory, isNot(directory(await second.resolveSymbolicLinks())));
    expect(firstDirectory, directory(await first.resolveSymbolicLinks()));
    expect(firstDirectory, directory(await alias.resolveSymbolicLinks()));
    expect(firstDirectory, startsWith('${dataHome.path}/'));
    expect(await dataHome.list().isEmpty, isTrue);
  });

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
        executable: '/opt/my_app/bin/my_app',
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
        executable: '/opt/my_app/bin/my_app',
        temporaryDirectory: 'relative/tmp',
      ),
      throwsArgumentError,
    );
  });

  test('private temporary fallback preserves UTF-8 directory paths', () async {
    final parent = await Directory('${bundle.path}/tmp-空间-\u{10400}').create();
    final directory = linuxDataDirectory(
      environment: {},
      executable: '/opt/my_app/bin/my_app',
      temporaryDirectory: parent.path,
    );
    expect(directory,
        startsWith('${await parent.resolveSymbolicLinks()}/sing-box-flutter-'));
    expect(await Directory(directory).exists(), isTrue);
    expect((await Directory(directory).stat()).mode & 0x1ff, 0x1c0);
  });

  test('temporary fallback rejects NUL paths before native path truncation',
      () async {
    final entries = await bundle.list().length;
    expect(
      () => linuxDataDirectory(
        environment: {},
        executable: '/opt/my_app/bin/my_app',
        temporaryDirectory: '${bundle.path}\u0000/private-path-secret',
      ),
      throwsA(isA<PlatformException>()
          .having((error) => error.code, 'code', 'state_directory')
          .having((error) => error.toString(), 'message',
              isNot(contains('private-path-secret')))),
    );
    expect(await bundle.list().length, entries);
  });

  test('fallback ignores precreated predictable paths and symlinks', () async {
    final target = await Directory('${bundle.path}/other-user').create();
    final marker =
        await File('${target.path}/marker').writeAsString('unchanged');
    final predictable = Directory('${bundle.path}/.local/share/my_app');
    await predictable.create(recursive: true);
    await Link('${predictable.path}/sing_box_flutter').create(target.path);
    final directory = linuxDataDirectory(
      environment: {'HOME': '', 'XDG_DATA_HOME': ''},
      executable: '/opt/my_app/bin/my_app',
      temporaryDirectory: bundle.path,
    );
    expect(directory, isNot('${predictable.path}/sing_box_flutter'));
    expect(directory, startsWith('${await bundle.resolveSymbolicLinks()}/'));
    expect(await Directory(directory).list().isEmpty, isTrue);
    expect(await marker.readAsString(), 'unchanged');
    expect(await target.list().length, 1);
  });

  test('temporary fallback failures report sanitized errors', () async {
    final invalidTemp = await File('${bundle.path}/private-path-secret')
        .writeAsString('not a directory');
    expect(
      () => linuxDataDirectory(
        environment: {},
        executable: '/opt/my_app/bin/my_app',
        temporaryDirectory: invalidTemp.path,
      ),
      throwsA(isA<PlatformException>()
          .having((error) => error.code, 'code', 'state_directory')
          .having((error) => error.toString(), 'message',
              isNot(contains('private-path-secret')))),
    );
  });

  test('a backend reuses its private fallback after helper replacement',
      () async {
    final dataDirectory = linuxDataDirectory(
      environment: {},
      executable: '/opt/my_app/bin/my_app',
      temporaryDirectory: bundle.path,
    );
    final launches = <List<String>>[];
    plugin = LinuxSingBoxFlutter.forTesting(
      bundleDirectory: bundle.path,
      dataDirectory: dataDirectory,
      startProcess: (_, args) async {
        launches.add(args);
        return child;
      },
    );
    await plugin.initialize(notificationTitle: 'Test');
    final failure = plugin.events.firstWhere(
      (event) => event is VpnStatus && event.state == VpnConnectionState.error,
    );
    child.exit();
    await failure;
    child = FakeCoreProcess();
    await plugin.initialize(notificationTitle: 'Test');
    expect(launches, [
      ['--data-dir', dataDirectory],
      ['--data-dir', dataDirectory],
    ]);
    expect(await Directory(dataDirectory).exists(), isTrue);
  });

  test('handshake verifies the bundled core before initialization', () async {
    expect(await plugin.getVersion(), '1.14.4');
    expect(child.requests.map((r) => r['method']), ['getInfo']);
    expect(() => plugin.stop(), throwsA(isA<PlatformException>()));
    await plugin.initialize(notificationTitle: 'Test');
    expect(executable, '${bundle.path}/sing-box-core');
    expect(arguments, ['--data-dir', '${bundle.path}/state']);
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
