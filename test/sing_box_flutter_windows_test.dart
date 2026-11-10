import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter/sing_box_flutter.dart';
import 'package:sing_box_flutter/sing_box_flutter_windows.dart';
import 'package:sing_box_flutter/src/windows_core_client.dart';

class _CoreProcess implements Process {
  _CoreProcess() {
    stdin = IOSink(_input.sink);
    _input.stream
        .transform(utf8.decoder)
        .transform(const LineSplitter())
        .listen((line) {
      final request = jsonDecode(line) as Map<String, dynamic>;
      requests.add(request);
      onRequest?.call(request);
    });
  }

  final _input = StreamController<List<int>>();
  final _output = StreamController<List<int>>();
  final _errors = StreamController<List<int>>();
  final _exit = Completer<int>();
  final requests = <Map<String, dynamic>>[];
  void Function(Map<String, dynamic>)? onRequest;
  String state = 'disconnected';
  bool initialized = false;
  bool killed = false;
  bool _outputObserved = false;
  bool _errorsObserved = false;

  void send(Object value) => _output.add(utf8.encode('${jsonEncode(value)}\n'));

  void respond(Map<String, dynamic> request, Object? result) =>
      send({'id': request['id'], 'result': result});

  void respondNormally(Map<String, dynamic> request) {
    if (request['method'] == 'initialize') initialized = true;
    if (!initialized && request['method'] != 'getVersion') {
      send({
        'id': request['id'],
        'error': {'code': 'not_initialized', 'message': 'Initialize first.'},
      });
      return;
    }
    respond(
        request,
        switch (request['method']) {
          'getVersion' => '1.14.4',
          'getStatus' => {'type': 'status', 'state': state},
          _ => null,
        });
  }

  void event(String state) {
    this.state = state;
    send({
      'event': {'type': 'status', 'state': state}
    });
  }

  Future<void> exit() async {
    if (!_outputObserved && !_errorsObserved) await stdin.close();
    // An unlaunched fake still needs consumers for single-subscription close
    // futures; cancelled streams have already been observed and need no drain.
    if (!_outputObserved) {
      _outputObserved = true;
      unawaited(_output.stream.drain<void>());
    }
    if (!_errorsObserved) {
      _errorsObserved = true;
      unawaited(_errors.stream.drain<void>());
    }
    if (!_exit.isCompleted) _exit.complete(0);
    await _output.close();
    await _errors.close();
  }

  @override
  late final IOSink stdin;
  @override
  Stream<List<int>> get stdout {
    _outputObserved = true;
    return _output.stream;
  }

  @override
  Stream<List<int>> get stderr {
    _errorsObserved = true;
    return _errors.stream;
  }

  @override
  Future<int> get exitCode => _exit.future;
  @override
  int get pid => 42;
  @override
  bool kill([ProcessSignal signal = ProcessSignal.sigterm]) {
    killed = true;
    unawaited(exit());
    return true;
  }
}

void main() {
  late _CoreProcess process;
  late List<_CoreProcess> processes;
  late SingBoxFlutterWindows plugin;
  late List<String> launches;

  setUp(() {
    process = _CoreProcess();
    processes = [process];
    launches = [];
    plugin = SingBoxFlutterWindows.withLauncher(
      launcher: (executable) async {
        final index = launches.length;
        launches.add(executable);
        return processes[index];
      },
      workingPath: r'C:\Users\Test\AppData\Local\app\sing_box_flutter',
      executable: r'C:\app\sing_box_flutter_core.exe',
    );
  });

  tearDown(() async {
    for (final process in processes) {
      await process.exit();
    }
  });

  test('concurrent calls share one core and configs only use the pipe',
      () async {
    process.onRequest = process.respondNormally;
    await Future.wait([
      plugin.initialize(
          applicationId: 'test.app', notificationTitle: 'Example'),
      plugin.getVersion(),
    ]);
    const config = '{"private_key":"private-secret"}';
    await plugin.checkConfig(config);
    expect(launches, [r'C:\app\sing_box_flutter_core.exe']);
    expect(process.requests.map((request) => request['method']),
        containsAll(['initialize', 'getVersion', 'checkConfig']));
    expect(process.requests.last['arguments'], {'config': config});
  });

  test('subscriptions replay state and cancellation never stops the core',
      () async {
    process.onRequest = process.respondNormally;
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    process.onRequest = (request) {
      if (request['method'] == 'start') {
        process.event('connecting');
        process.respond(request, null);
        process.event('connected');
      } else {
        process.respond(request, null);
      }
    };
    final events = <VpnConnectionState>[];
    final subscription = plugin.events.cast<VpnStatus>().listen(
          (event) => events.add(event.state),
        );
    await plugin.start(config: '{}', name: 'test');
    await Future<void>.delayed(Duration.zero);
    expect(events, [
      VpnConnectionState.disconnected,
      VpnConnectionState.connecting,
      VpnConnectionState.connected,
    ]);
    await subscription.cancel();
    expect((await plugin.events.first as VpnStatus).isConnected, isTrue);
    expect(process.requests.last['method'], 'start');
    expect(process.requests.where((request) => request['method'] == 'stop'),
        isEmpty);
    expect(process.killed, isFalse);
  });

  test('helper errors retain stable platform codes', () async {
    process.onRequest = process.respondNormally;
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    process.onRequest = (request) => process.send({
          'id': request['id'],
          'error': {
            'code': 'permission_denied',
            'message': 'Run the Windows application as administrator.',
          },
        });
    await expectLater(
        plugin.start(config: '{}', name: 'test'),
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'permission_denied')));
  });

  test('exiting core fails outstanding calls and emits a status error',
      () async {
    final waiting = plugin.getVersion();
    final assertion = expectLater(waiting, throwsA(isA<PlatformException>()));
    await Future<void>.delayed(Duration.zero);
    await process.exit();
    await assertion;
    expect((await plugin.events.first as VpnStatus).state,
        VpnConnectionState.error);
  });

  test('malformed diagnostics never expose the received input', () async {
    process.onRequest =
        (_) => process._output.add(utf8.encode('secret-auth-key not JSON\n'));
    await expectLater(
        plugin.getVersion(),
        throwsA(isA<PlatformException>().having((error) => error.toString(),
            'safe error', isNot(contains('secret-auth-key')))));
    expect(process.killed, isFalse);
  });

  test('timeout reports failure while letting helper clean up', () async {
    var failed = false;
    final client = WindowsCoreClient(process,
        onEvent: (_) {},
        onFailure: () => failed = true,
        requestTimeout: const Duration(milliseconds: 10));
    await expectLater(
        client.invoke('getVersion'), throwsA(isA<PlatformException>()));
    await process.stdin.done;
    expect(failed, isTrue);
    expect(process._exit.isCompleted, isFalse);
    expect(process.killed, isFalse);
  });

  test('replacement waits for actual exit and shares one pending launch',
      () async {
    process.onRequest = process.respondNormally;
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    final failure = plugin.events.cast<VpnStatus>().firstWhere(
          (status) => status.state == VpnConnectionState.error,
        );
    process._output.add(utf8.encode('malformed protocol\n'));
    await failure;
    await process.stdin.done;

    final replacement = _CoreProcess();
    replacement.onRequest = replacement.respondNormally;
    processes.add(replacement);
    final reconnect = plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    final concurrent = plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    await Future<void>.delayed(Duration.zero);
    expect(launches, hasLength(1));
    expect(process._exit.isCompleted, isFalse);

    await process.exit();
    await Future.wait([reconnect, concurrent]);
    expect(launches, hasLength(2));
    expect(process.killed, isFalse);
    expect((await plugin.events.first as VpnStatus).state,
        VpnConnectionState.disconnected);
  });

  test('reinitializing a replacement refreshes existing and replayed status',
      () async {
    process.onRequest = process.respondNormally;
    final states = <VpnConnectionState>[];
    final subscription = plugin.events.cast<VpnStatus>().listen(
          (status) => states.add(status.state),
        );
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    await process.exit();
    await Future<void>.delayed(Duration.zero);
    expect(states.last, VpnConnectionState.error);

    final replacement = _CoreProcess();
    replacement.onRequest = replacement.respondNormally;
    processes.add(replacement);
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    await Future<void>.delayed(Duration.zero);
    expect(states.last, VpnConnectionState.disconnected);
    expect((await plugin.events.first as VpnStatus).state,
        VpnConnectionState.disconnected);
    expect((await plugin.getStatus()).state, VpnConnectionState.disconnected);
    await subscription.cancel();
  });

  test('an awaited status snapshot never replaces a newer native event',
      () async {
    process.onRequest = process.respondNormally;
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    process.onRequest = (request) {
      // One OS read can carry both an older RPC response and the newer event.
      final response = jsonEncode({
        'id': request['id'],
        'result': {'type': 'status', 'state': 'disconnected'},
      });
      final event = jsonEncode({
        'event': {'type': 'status', 'state': 'connected'},
      });
      process._output.add(utf8.encode('$response\n$event\n'));
    };
    expect((await plugin.getStatus()).state, VpnConnectionState.disconnected);
    expect((await plugin.events.first as VpnStatus).state,
        VpnConnectionState.connected);
  });

  test(
      'control methods require explicit initialization without launching a helper',
      () async {
    for (final operation in <Future<Object?> Function()>[
      plugin.getStatus,
      () => plugin.checkConfig('{}'),
      plugin.requestPermission,
      () => plugin.start(config: '{}', name: 'test'),
      plugin.stop,
    ]) {
      await expectLater(
          operation(),
          throwsA(isA<PlatformException>()
              .having((error) => error.code, 'code', 'not_initialized')));
    }
    expect(launches, isEmpty);
  });

  test(
      'status refresh and stop after helper loss never launch an uninitialized core',
      () async {
    process.onRequest = process.respondNormally;
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    await process.exit();
    expect((await plugin.getStatus()).state, VpnConnectionState.error);
    await expectLater(
        plugin.start(config: '{}', name: 'test'),
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'not_initialized')));
    await plugin.stop();
    expect((await plugin.getStatus()).state, VpnConnectionState.disconnected);
    expect(launches, hasLength(1));
    expect(process.requests.map((request) => request['method']),
        ['initialize', 'getStatus']);
  });

  test(
      'version-only replacement remains uninitialized until initialize succeeds',
      () async {
    process.onRequest = process.respondNormally;
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    await process.exit();
    final replacement = _CoreProcess();
    replacement.onRequest = replacement.respondNormally;
    processes.add(replacement);
    expect(await plugin.getVersion(), '1.14.4');
    expect(replacement.initialized, isFalse);
    expect((await plugin.getStatus()).state, VpnConnectionState.error);
    await expectLater(
        plugin.checkConfig('{}'),
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'not_initialized')));
    expect(replacement.requests.map((request) => request['method']),
        ['getVersion']);
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    expect(replacement.initialized, isTrue);
    await plugin.checkConfig('{}');
    expect((await plugin.getStatus()).state, VpnConnectionState.disconnected);
  });

  test('oversized UTF-8 requests do not disconnect an existing connection',
      () async {
    process.onRequest = process.respondNormally;
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    final connected = plugin.events.cast<VpnStatus>().firstWhere(
          (status) => status.isConnected,
        );
    process.event('connected');
    await connected;
    final config = jsonEncode({
      'outbounds': [
        {'type': 'direct', 'tag': 'direct'}
      ],
      'route': {
        'rules': [
          {
            'domain_suffix': ['中' * (WindowsCoreClient.maxRequestBytes ~/ 2)],
            'action': 'route',
            'outbound': 'direct'
          },
        ],
        'final': 'direct',
      },
    });
    expect(config.length, lessThan(WindowsCoreClient.maxRequestBytes));
    final sent = process.requests.length;
    await expectLater(
        plugin.checkConfig(config),
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'request_too_large')));
    expect(process.requests, hasLength(sent));
    expect((await plugin.getStatus()).isConnected, isTrue);
    expect(await plugin.getVersion(), '1.14.4');
    expect(launches, hasLength(1));
    expect(process._exit.isCompleted, isFalse);
  });

  test('request limit includes the newline and leaves the transport usable',
      () async {
    var failures = 0;
    process.onRequest = (request) => process.respond(request, null);
    final client = WindowsCoreClient(process,
        onEvent: (_) {}, onFailure: () => failures++);
    final overhead = utf8
        .encode('${jsonEncode({
              'id': 1,
              'method': 'checkConfig',
              'arguments': {'config': ''},
            })}\n')
        .length;
    final payload = 'x' * (WindowsCoreClient.maxRequestBytes - overhead);
    await client.invoke('checkConfig', {'config': payload});
    await expectLater(
        client.invoke('checkConfig', {'config': '${payload}x'}),
        throwsA(isA<PlatformException>()
            .having((error) => error.code, 'code', 'request_too_large')));
    await client.invoke('getVersion');
    expect(process.requests.map((request) => request['method']),
        ['checkConfig', 'getVersion']);
    expect(failures, 0);
  });

  test('application storage uses stable case-sensitive identifiers', () {
    final first =
        SingBoxFlutterWindows.applicationStorageName('dev.example.Vpn');
    expect(
        first, SingBoxFlutterWindows.applicationStorageName('dev.example.Vpn'));
    expect(first,
        isNot(SingBoxFlutterWindows.applicationStorageName('dev.example.vpn')));
    expect(first, matches(RegExp(r'^v1-dev_example_Vpn-[a-f0-9]{64}$')));
    expect(SingBoxFlutterWindows.applicationStorageName('a' * 128).length,
        lessThanOrEqualTo(92));
    for (final invalid in ['', ' app', 'app ', 'app/id', 'a' * 129]) {
      expect(() => SingBoxFlutterWindows.applicationStorageName(invalid),
          throwsArgumentError);
    }
  });

  test('Windows requires an application ID before starting any helper',
      () async {
    await expectLater(
        plugin.initialize(notificationTitle: 'Example'), throwsArgumentError);
    expect(launches, isEmpty);
  });

  test('versioned installation paths keep the same persistent namespace',
      () async {
    final directories = <Object?>[];
    for (final executable in [
      r'C:\Apps\app-1.2.3\sing_box_flutter_core.exe',
      r'C:\Apps\app-1.2.4\sing_box_flutter_core.exe',
    ]) {
      final current = _CoreProcess();
      current.onRequest = current.respondNormally;
      processes.add(current);
      final backend = SingBoxFlutterWindows.withLauncher(
        launcher: (_) async => current,
        localAppData: r'C:\Users\Test\AppData\Local',
        executable: executable,
      );
      await backend.initialize(
          applicationId: 'dev.example.Vpn', notificationTitle: 'Example');
      directories
          .add((current.requests.first['arguments'] as Map)['workingPath']);
      await current.exit();
    }
    expect(directories.first, directories.last);
  });

  test('case-sensitive installations retain distinct caller identities',
      () async {
    final directories = <Object?>[];
    for (final entry in [
      (r'C:\Apps\Product\core.exe', 'dev.example.Product'),
      (r'C:\Apps\product\core.exe', 'dev.example.product'),
    ]) {
      final current = _CoreProcess();
      current.onRequest = current.respondNormally;
      processes.add(current);
      final backend = SingBoxFlutterWindows.withLauncher(
        launcher: (_) async => current,
        localAppData: r'C:\Users\Test\AppData\Local',
        executable: entry.$1,
      );
      await backend.initialize(
          applicationId: entry.$2, notificationTitle: 'Example');
      directories
          .add((current.requests.first['arguments'] as Map)['workingPath']);
      await current.exit();
    }
    expect(directories.first, isNot(directories.last));
  });

  test('repeated initialization preserves the native connected state',
      () async {
    process.onRequest = process.respondNormally;
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    final connected = plugin.events.cast<VpnStatus>().firstWhere(
          (status) => status.isConnected,
        );
    process.event('connected');
    await connected;
    await plugin.initialize(
        applicationId: 'test.app', notificationTitle: 'Example');
    expect((await plugin.events.first as VpnStatus).isConnected, isTrue);
  });
}
