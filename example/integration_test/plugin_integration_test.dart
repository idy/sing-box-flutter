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
}
