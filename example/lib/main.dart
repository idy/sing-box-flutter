import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:sing_box_flutter/sing_box_flutter.dart';

void main() => runApp(const ExampleApp());

/// A minimal host application for the native VPN plugin.
class ExampleApp extends StatelessWidget {
  const ExampleApp({super.key});

  @override
  Widget build(BuildContext context) => MaterialApp(
        title: 'sing_box_flutter',
        theme: ThemeData(colorSchemeSeed: Colors.indigo),
        home: const VpnExample(),
      );
}

class VpnExample extends StatefulWidget {
  const VpnExample({super.key});

  @override
  State<VpnExample> createState() => _VpnExampleState();
}

class _VpnExampleState extends State<VpnExample> {
  static const applicationId = String.fromEnvironment('APPLICATION_ID',
      defaultValue: 'dev.idy.singboxflutter.example');
  static const appGroup = String.fromEnvironment('APP_GROUP');
  static const provider = String.fromEnvironment('TUNNEL_BUNDLE_ID');
  final _plugin = SingBoxFlutter();
  final _config = TextEditingController(text: defaultConfiguration);
  StreamSubscription<VpnStatus>? _subscription;
  VpnStatus _status = const VpnStatus(state: VpnConnectionState.disconnected);
  String? _error;
  String? _version;
  bool _working = false;
  bool _initialized = false;

  Future<void> _perform(Future<void> Function() operation) async {
    setState(() {
      _working = true;
      _error = null;
    });
    try {
      await operation();
    } on PlatformException catch (error) {
      if (mounted) {
        setState(() => _error = error.message ?? error.code);
      }
    } catch (error) {
      if (mounted) setState(() => _error = error.toString());
    } finally {
      if (mounted) setState(() => _working = false);
    }
  }

  Future<void> _initialize() async {
    if (_initialized) return;
    if (defaultTargetPlatform == TargetPlatform.iOS &&
        (appGroup.isEmpty || provider.isEmpty)) {
      throw StateError(
        'Configure the Packet Tunnel target, then set APP_GROUP and '
        'TUNNEL_BUNDLE_ID with --dart-define. See doc/ios-setup.md.',
      );
    }
    await _plugin.initialize(
      applicationId: applicationId,
      appGroup: appGroup.isEmpty ? null : appGroup,
      providerBundleIdentifier: provider.isEmpty ? null : provider,
      notificationTitle: 'sing_box_flutter example',
    );
    _subscription = _plugin.status.listen(
      (status) {
        if (mounted) setState(() => _status = status);
      },
      onError: (Object error) {
        if (mounted) setState(() => _error = 'Native status unavailable.');
      },
    );
    final version = await _plugin.getVersion();
    final status = await _plugin.getStatus();
    if (mounted) {
      setState(() {
        _version = version;
        _status = status;
        _initialized = true;
      });
    }
  }

  Future<void> _connect() async {
    await _initialize();
    await _plugin.checkConfig(_config.text);
    if (!await _plugin.requestPermission()) {
      throw StateError('VPN permission was not granted.');
    }
    await _plugin.start(config: _config.text, name: 'sing_box_flutter example');
  }

  @override
  void dispose() {
    // The service intentionally survives UI disposal. Disconnect explicitly.
    unawaited(_subscription?.cancel());
    _config.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final active = _status.state == VpnConnectionState.connected ||
        _status.state == VpnConnectionState.connecting;
    return Scaffold(
      appBar: AppBar(title: const Text('sing_box_flutter')),
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.all(20),
          children: [
            Text(
              'VPN: ${_status.state.name}',
              style: Theme.of(context).textTheme.titleLarge,
            ),
            if (_version != null) Text('libbox $_version'),
            const SizedBox(height: 12),
            const Text(
              'This example creates a VPN tunnel with direct routing. '
              'Replace the JSON to use your own sing-box routing and endpoints. '
              'Install the native core before building the app.',
            ),
            const SizedBox(height: 20),
            TextField(
              controller: _config,
              enabled: !active && !_working,
              minLines: 12,
              maxLines: 22,
              autocorrect: false,
              enableSuggestions: false,
              style: const TextStyle(fontFamily: 'monospace', fontSize: 13),
              decoration: const InputDecoration(
                labelText: 'sing-box JSON',
                border: OutlineInputBorder(),
                alignLabelWithHint: true,
              ),
            ),
            const SizedBox(height: 16),
            FilledButton(
              onPressed: _working
                  ? null
                  : () => _perform(active ? _plugin.stop : _connect),
              child: Text(
                _working
                    ? 'Working…'
                    : active
                        ? 'Disconnect'
                        : 'Connect',
              ),
            ),
            TextButton(
              onPressed: _working
                  ? null
                  : () => _perform(() async {
                        await _initialize();
                        await _plugin.checkConfig(_config.text);
                      }),
              child: const Text('Validate configuration'),
            ),
            if (_status.message != null) Text(_status.message!),
            if (_error != null)
              Padding(
                padding: const EdgeInsets.only(top: 12),
                child: Text(
                  _error!,
                  style: TextStyle(color: Theme.of(context).colorScheme.error),
                ),
              ),
          ],
        ),
      ),
    );
  }
}

const defaultConfiguration = '''
{
  "log": {"level": "warn"},
  "dns": {
    "servers": [{"type": "udp", "tag": "dns-direct", "server": "1.1.1.1"}],
    "final": "dns-direct"
  },
  "inbounds": [{
    "type": "tun",
    "tag": "tun-in",
    "address": ["172.19.0.1/30", "fdfe:dcba:9876::1/126"],
    "auto_route": true,
    "stack": "gvisor"
  }],
  "outbounds": [{"type": "direct", "tag": "direct"}],
  "route": {
    "auto_detect_interface": true,
    "rules": [{"protocol": "dns", "action": "hijack-dns"}],
    "final": "direct"
  }
}
''';
