import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter_example/main.dart';

void main() {
  testWidgets('opening the example never starts the VPN', (tester) async {
    await tester.pumpWidget(const ExampleApp());
    expect(find.text('VPN: disconnected'), findsOneWidget);
    expect(find.text('sing-box JSON'), findsOneWidget);
    // No native channel is mocked: an automatic start would fail this test.
    expect(tester.takeException(), isNull);
  });
}
