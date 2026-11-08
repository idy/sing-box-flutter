import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter/sing_box_flutter.dart';
import 'package:sing_box_flutter/sing_box_flutter_method_channel.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('dev.idy.sing_box_flutter/methods');
  final platform = MethodChannelSingBoxFlutter();
  Object? response;
  PlatformException? failure;

  setUp(() {
    response = null;
    failure = null;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, (call) async {
      if (failure != null) throw failure!;
      return response;
    });
  });

  tearDown(() {
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, null);
  });

  test('missing permission result fails closed', () async {
    expect(await platform.requestPermission(), isFalse);
  });

  test('version comes from native libbox', () async {
    response = '1.14.4';
    expect(await platform.getVersion(), '1.14.4');
    response = null;
    await expectLater(platform.getVersion(), throwsStateError);
  });

  test('status reflects native failure instead of reporting connected',
      () async {
    response = {'state': 'error', 'message': 'Tunnel failed to start.'};
    final status = await platform.getStatus();
    expect(status.state, VpnConnectionState.error);
    expect(status.isConnected, isFalse);
    expect(status.message, 'Tunnel failed to start.');
  });

  test('missing native status is an explicit error', () async {
    await expectLater(platform.getStatus(), throwsStateError);
  });

  test('native start failures propagate to the caller', () async {
    failure = PlatformException(code: 'permission_denied');
    await expectLater(
      platform.start(config: '{}', name: 'Example'),
      throwsA(isA<PlatformException>().having(
        (error) => error.code,
        'code',
        'permission_denied',
      )),
    );
  });
}
