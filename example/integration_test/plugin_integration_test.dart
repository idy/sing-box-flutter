import 'package:flutter/foundation.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:sing_box_flutter/sing_box_flutter.dart';

void main() {
  IntegrationTestWidgetsFlutterBinding.ensureInitialized();

  testWidgets('native libbox is linked and exposes its version', (
    tester,
  ) async {
    final version = await SingBoxFlutter().getVersion();
    expect(version, isNotEmpty);
    expect(version, isNot(contains('mock')));
  });

  testWidgets('Linux backend initializes and validates without TUN privileges',
      (
    tester,
  ) async {
    if (defaultTargetPlatform != TargetPlatform.linux) return;
    final plugin = SingBoxFlutter();
    await plugin.initialize();
    await plugin.checkConfig('{"outbounds":[{"type":"direct"}]}');
    expect((await plugin.getStatus()).state, VpnConnectionState.disconnected);
    await plugin.stop();
  });
}
