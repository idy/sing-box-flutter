import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter/sing_box_flutter.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('dev.idy.sing_box_flutter/methods');
  final calls = <MethodCall>[];
  final plugin = SingBoxFlutter();

  setUp(() {
    calls.clear();
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, (call) async {
      calls.add(call);
      if (call.method == 'requestPermission') return true;
      return null;
    });
  });

  tearDown(() {
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, null);
  });

  test('initialization never starts a connection', () async {
    await plugin.initialize(
      applicationId: 'dev.idy.example',
      appGroup: 'group.dev.idy.example',
      providerBundleIdentifier: 'dev.idy.example.PacketTunnel',
    );
    expect(calls.map((call) => call.method), ['initialize']);
    expect(calls.single.arguments, {
      'appGroup': 'group.dev.idy.example',
      'providerBundleIdentifier': 'dev.idy.example.PacketTunnel',
      'notificationTitle': 'sing-box',
    });
  });

  test('invalid JSON never reaches native code or leaks config in errors', () {
    const secret = '{"auth_key":"tskey-secret-that-must-not-leak"';
    expect(
      () => plugin.start(config: secret),
      throwsA(isA<FormatException>().having(
        (error) => error.toString().contains('tskey-secret'),
        'contains credentials',
        isFalse,
      )),
    );
    expect(calls, isEmpty);
  });

  test('non-object JSON and invalid names are rejected before starting', () {
    for (final config in ['[]', 'null', '42', '"text"']) {
      expect(() => plugin.checkConfig(config), throwsFormatException);
    }
    expect(() => plugin.start(config: '{}', name: ''), throwsArgumentError);
    expect(() => plugin.initialize(appGroup: ' '), throwsArgumentError);
    expect(
        () => plugin.initialize(applicationId: 'app/id'), throwsArgumentError);
    expect(calls, isEmpty);
  });

  test('permission, start and explicit stop are separate operations', () async {
    expect(await plugin.requestPermission(), isTrue);
    await plugin.start(config: '{"inbounds":[]}', name: 'Example');
    await plugin.stop();
    expect(calls.map((call) => call.method), [
      'requestPermission',
      'start',
      'stop',
    ]);
    expect(calls[1].arguments, {
      'config': '{"inbounds":[]}',
      'name': 'Example',
    });
  });

  test('status parsing preserves each native lifecycle state', () {
    for (final state in VpnConnectionState.values) {
      final status = SingBoxEvent.fromMap({
        'type': 'status',
        'state': state.name,
      }) as VpnStatus;
      expect(status.state, state);
      expect(status.isConnected, state == VpnConnectionState.connected);
    }
  });

  test('unknown native events and states do not masquerade as disconnected',
      () {
    expect(
      () => SingBoxEvent.fromMap({'type': 'unknown'}),
      throwsFormatException,
    );
    expect(
      () => VpnStatus.fromMap({'state': 'unknown'}),
      throwsFormatException,
    );
    expect(
      () => VpnStatus.fromMap({'state': 'connected', 'message': 1}),
      throwsFormatException,
    );
  });
}
