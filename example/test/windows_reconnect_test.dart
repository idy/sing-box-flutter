import 'dart:async';

import 'package:flutter/services.dart';
import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter/sing_box_flutter.dart';
import 'package:sing_box_flutter/sing_box_flutter_platform_interface.dart';
import 'package:sing_box_flutter_example/main.dart';

class _RestartablePlatform extends SingBoxFlutterPlatform {
  final updates = StreamController<SingBoxEvent>.broadcast();
  VpnStatus status = const VpnStatus(state: VpnConnectionState.disconnected);
  bool initialized = false;
  int initializeCalls = 0;
  int startCalls = 0;
  int eventSubscriptions = 0;

  @override
  Future<void> initialize({
    String? appGroup,
    String? providerBundleIdentifier,
    required String notificationTitle,
  }) async {
    initializeCalls++;
    initialized = true;
    status = const VpnStatus(state: VpnConnectionState.disconnected);
  }

  @override
  Future<String> getVersion() async => 'test-core';

  @override
  Future<VpnStatus> getStatus() async => status;

  @override
  Future<void> checkConfig(String config) async {
    if (!initialized) {
      throw PlatformException(code: 'not_initialized');
    }
  }

  @override
  Future<bool> requestPermission() async => true;

  @override
  Future<void> start({required String config, required String name}) async {
    startCalls++;
    status = const VpnStatus(state: VpnConnectionState.connected);
    updates.add(status);
  }

  @override
  Future<void> stop() async {
    status = const VpnStatus(state: VpnConnectionState.disconnected);
    updates.add(status);
  }

  void loseHelper() {
    initialized = false;
    status = const VpnStatus(
      state: VpnConnectionState.error,
      message: 'The Windows VPN core exited or stopped responding.',
    );
    updates.add(status);
  }

  @override
  Stream<SingBoxEvent> get events {
    eventSubscriptions++;
    return updates.stream;
  }
}

void main() {
  testWidgets(
      'Windows reconnect initializes a new helper with one subscription',
      (tester) async {
    final original = SingBoxFlutterPlatform.instance;
    final platform = _RestartablePlatform();
    SingBoxFlutterPlatform.instance = platform;
    addTearDown(() async {
      SingBoxFlutterPlatform.instance = original;
      await platform.updates.close();
    });

    await tester.binding.setSurfaceSize(const Size(1000, 1400));
    addTearDown(() => tester.binding.setSurfaceSize(null));
    await tester.pumpWidget(const ExampleApp());
    await tester.tap(find.text('Connect'));
    await tester.pumpAndSettle();
    expect(find.text('VPN: connected'), findsOneWidget);
    expect(platform.initializeCalls, 1);

    for (var failure = 0; failure < 2; failure++) {
      platform.loseHelper();
      await tester.pumpAndSettle();
      expect(find.text('VPN: error'), findsOneWidget);
      await tester.tap(find.text('Connect'));
      await tester.pumpAndSettle();
      expect(find.text('VPN: connected'), findsOneWidget);
      expect(platform.initializeCalls, failure + 2);
      expect(platform.startCalls, failure + 2);
      expect(platform.eventSubscriptions, 1);
      expect(find.text('not_initialized'), findsNothing);
    }
    await tester.pumpWidget(const SizedBox.shrink());
  }, variant: TargetPlatformVariant.only(TargetPlatform.windows));
}
