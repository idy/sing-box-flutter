import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter/sing_box_flutter.dart';
import 'package:sing_box_flutter/sing_box_flutter_platform_interface.dart';
import 'package:sing_box_flutter_example/main.dart';

class PendingStartPlatform extends SingBoxFlutterPlatform {
  final updates = StreamController<SingBoxEvent>.broadcast();
  final started = Completer<void>();
  final stopped = Completer<void>();
  int stops = 0;

  @override
  Future<void> initialize(
      {String? appGroup,
      String? providerBundleIdentifier,
      required String notificationTitle}) async {}
  @override
  Future<String> getVersion() async => 'test';
  @override
  Future<VpnStatus> getStatus() async =>
      const VpnStatus(state: VpnConnectionState.disconnected);
  @override
  Future<void> checkConfig(String config) async {}
  @override
  Future<bool> requestPermission() async => true;
  @override
  Future<void> start({required String config, required String name}) {
    updates.add(const VpnStatus(state: VpnConnectionState.connecting));
    return started.future;
  }

  @override
  Future<void> stop() async {
    stops++;
    updates.add(const VpnStatus(state: VpnConnectionState.disconnecting));
    await stopped.future;
    updates.add(const VpnStatus(state: VpnConnectionState.disconnected));
  }

  @override
  Stream<SingBoxEvent> get events => updates.stream;
}

void main() {
  testWidgets('opening the example never starts the VPN', (tester) async {
    await tester.pumpWidget(const ExampleApp());
    expect(find.text('VPN: disconnected'), findsOneWidget);
    expect(find.text('sing-box JSON'), findsOneWidget);
    // No native channel is mocked: an automatic start would fail this test.
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'disconnect cancels a pending Linux start and stays busy during stop',
      (tester) async {
    final previous = SingBoxFlutterPlatform.instance;
    final backend = PendingStartPlatform();
    SingBoxFlutterPlatform.instance = backend;
    debugDefaultTargetPlatformOverride = TargetPlatform.linux;
    addTearDown(() async {
      SingBoxFlutterPlatform.instance = previous;
      debugDefaultTargetPlatformOverride = null;
      await backend.updates.close();
    });
    await tester.binding.setSurfaceSize(const Size(1200, 2000));
    addTearDown(() => tester.binding.setSurfaceSize(null));
    await tester.pumpWidget(const ExampleApp());
    await tester.pumpAndSettle();
    await tester.tap(find.text('Connect'));
    await tester.pumpAndSettle();
    expect(find.text('VPN: connecting'), findsOneWidget);
    final disconnect = find.widgetWithText(FilledButton, 'Disconnect');
    expect(tester.widget<FilledButton>(disconnect).onPressed, isNotNull);
    await tester.tap(disconnect);
    await tester.pumpAndSettle();
    expect(backend.stops, 1);
    expect(tester.widget<FilledButton>(find.byType(FilledButton)).onPressed,
        isNull);
    // Completing the original start must not re-enable controls while stop waits.
    backend.started.complete();
    await tester.pumpAndSettle();
    expect(tester.widget<FilledButton>(find.byType(FilledButton)).onPressed,
        isNull);
    backend.stopped.complete();
    await tester.pumpAndSettle();
    expect(find.text('VPN: disconnected'), findsOneWidget);
    expect(
        tester
            .widget<FilledButton>(find.widgetWithText(FilledButton, 'Connect'))
            .onPressed,
        isNotNull);
    expect(tester.takeException(), isNull);
    await tester.pumpWidget(const SizedBox.shrink());
    debugDefaultTargetPlatformOverride = null;
  });
}
