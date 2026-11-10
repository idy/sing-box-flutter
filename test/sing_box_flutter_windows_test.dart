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
  bool killed = false;

  void send(Object value) => _output.add(utf8.encode('${jsonEncode(value)}\n'));

  void respond(Map<String, dynamic> request, Object? result) =>
      send({'id': request['id'], 'result': result});

  void respondNormally(Map<String, dynamic> request) {
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
    if (!_exit.isCompleted) _exit.complete(0);
    await _output.close();
    await _errors.close();
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
      plugin.initialize(notificationTitle: 'Example'),
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
    expect(process.requests.map((request) => request['method']), ['start']);
    expect(process.killed, isFalse);
  });

  test('helper errors retain stable platform codes', () async {
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
    await plugin.initialize(notificationTitle: 'Example');
    final failure = plugin.events.cast<VpnStatus>().firstWhere(
          (status) => status.state == VpnConnectionState.error,
        );
    process._output.add(utf8.encode('malformed protocol\n'));
    await failure;
    await process.stdin.done;

    final replacement = _CoreProcess();
    replacement.onRequest = replacement.respondNormally;
    processes.add(replacement);
    final reconnect = plugin.initialize(notificationTitle: 'Example');
    final concurrent = plugin.initialize(notificationTitle: 'Example');
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
    await plugin.initialize(notificationTitle: 'Example');
    await process.exit();
    await Future<void>.delayed(Duration.zero);
    expect(states.last, VpnConnectionState.error);

    final replacement = _CoreProcess();
    replacement.onRequest = replacement.respondNormally;
    processes.add(replacement);
    await plugin.initialize(notificationTitle: 'Example');
    await Future<void>.delayed(Duration.zero);
    expect(states.last, VpnConnectionState.disconnected);
    expect((await plugin.events.first as VpnStatus).state,
        VpnConnectionState.disconnected);
    expect((await plugin.getStatus()).state, VpnConnectionState.disconnected);
    await subscription.cancel();
  });

  test('an awaited status snapshot never replaces a newer native event',
      () async {
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

  test('repeated initialization preserves the native connected state',
      () async {
    process.onRequest = process.respondNormally;
    await plugin.initialize(notificationTitle: 'Example');
    final connected = plugin.events.cast<VpnStatus>().firstWhere(
          (status) => status.isConnected,
        );
    process.event('connected');
    await connected;
    await plugin.initialize(notificationTitle: 'Example');
    expect((await plugin.events.first as VpnStatus).isConnected, isTrue);
  });
}
