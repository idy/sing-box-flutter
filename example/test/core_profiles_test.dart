import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:yaml/yaml.dart';

import '../tool/select_core_profile.dart';

const host = '''# Keep the app description.
name: example
version: 1.0.0+1
dependencies:
  flutter:
    sdk: flutter
  sing_box_flutter:
    path: ../
# Native feature selection follows.
sing_box_flutter:
  core:
    inbounds: [tun]
    outbounds: [direct]
    endpoints: [wgc]
    dns: [udp]
# Keep this Flutter comment too.
flutter:
  config:
    enable-swift-package-manager: false
  uses-material-design: true
''';

const aliasedHosts = {
  'selected alias': '''name: example
preset: &preset
  core:
    endpoints: [wgc]
sing_box_flutter: *preset
flutter:
  uses-material-design: true
''',
  'nonadjacent selected alias': '''name: example
preset: &preset
  core:
    endpoints: [wgc]
version: 1.0.0
sing_box_flutter: *preset
flutter:
  uses-material-design: true
''',
  'selected anchor reused elsewhere': '''name: example
sing_box_flutter: &preset
  core:
    endpoints: [wgc]
other_selection: *preset
flutter:
  uses-material-design: true
''',
};

Future<ProcessResult> runProfileFixture(Directory fixture) async {
  final example = Directory.current.absolute;
  final tool = Directory('${fixture.path}/tool')..createSync();
  final selector = File('${example.path}/tool/select_core_profile.dart');
  final wrapper = File('${tool.path}/invoke_real_main.dart')
    ..writeAsStringSync("import '${selector.uri}' as selector;\n"
        'void main(List<String> args) => selector.main(args);\n');
  final packageConfig = File('${example.path}/.dart_tool/package_config.json');
  final packages =
      jsonDecode(packageConfig.readAsStringSync())['packages'] as List;
  final flutter = Directory.fromUri(packageConfig.uri.resolve(
      packages.singleWhere((entry) => entry['name'] == 'flutter')['rootUri']
          as String));
  final dart = File.fromUri(flutter.parent.parent.uri.resolve(
      'bin/cache/dart-sdk/bin/${Platform.isWindows ? 'dart.exe' : 'dart'}'));
  return Process.run(
      dart.path,
      [
        '--packages=${example.path}/.dart_tool/package_config.json',
        wrapper.path,
        'wgc-minimal',
      ],
      workingDirectory: fixture.path);
}

Map<String, dynamic> withoutSelection(String text) {
  final map = Map<String, dynamic>.from(jsonDecode(jsonEncode(loadYaml(text))));
  map.remove('sing_box_flutter');
  return map;
}

void main() {
  for (final name in coreProfiles.keys) {
    test('$name replaces only the feature block and is idempotent', () {
      final profile = File('core-profiles/$name.yaml').readAsStringSync();
      final updated = applyCoreProfile(host, profile);
      expect(withoutSelection(updated), withoutSelection(host));
      expect(updated, contains('# Keep the app description.'));
      expect(updated, contains('# Keep this Flutter comment too.'));
      expect(applyCoreProfile(updated, profile), updated);
      expect(loadYaml(updated)['sing_box_flutter'],
          loadYaml(profile)['sing_box_flutter']);
    });
  }

  test('profiles can be switched repeatedly without accumulating blocks', () {
    var text = host;
    for (final name in ['singbox-full', 'wgc-tailscale', 'wgc-minimal']) {
      text = applyCoreProfile(
          text, File('core-profiles/$name.yaml').readAsStringSync());
    }
    final selection = loadYaml(text)['sing_box_flutter']['core'];
    expect(selection['endpoints'], ['wgc']);
    expect(withoutSelection(text), withoutSelection(host));
    expect(
        RegExp(r'^sing_box_flutter:', multiLine: true).allMatches(text).length,
        1);
  });

  test('can add a selection to an existing app without one', () {
    const text = 'name: host\nflutter:\n  uses-material-design: true\n';
    final profile = File('core-profiles/wgc-minimal.yaml').readAsStringSync();
    final updated = applyCoreProfile(text, profile);
    expect(withoutSelection(updated), withoutSelection(text));
    expect(loadYaml(updated)['sing_box_flutter']['core']['endpoints'], ['wgc']);
  });

  test('malformed or unknown profile settings cannot rewrite the app', () {
    for (final profile in [
      'name: not_a_profile',
      'sing_box_flutter:\n  core:\n    endpoints: [wgcc]',
      'sing_box_flutter:\n  core: {}\n  typo: true',
      'sing_box_flutter:\n  core: {}\nflutter: {}',
    ]) {
      expect(() => applyCoreProfile(host, profile), throwsFormatException);
    }
  });

  for (final entry in aliasedHosts.entries) {
    test('${entry.key} is rejected without redirecting its anchor', () {
      final profile = File('core-profiles/wgc-minimal.yaml').readAsStringSync();
      expect(
          () => applyCoreProfile(entry.value, profile),
          throwsA(isA<FormatException>().having(
              (error) => error.message, 'message', contains('YAML aliases'))));
    });
  }

  test('shared nodes in profiles and malformed candidate YAML fail safely', () {
    const aliasedProfile = '''sing_box_flutter:
  core:
    dns: &empty []
    endpoints: *empty
''';
    expect(() => applyCoreProfile(host, aliasedProfile), throwsFormatException);
    final profile = File('core-profiles/wgc-minimal.yaml').readAsStringSync();
    expect(() => applyCoreProfile('name: [unterminated', profile),
        throwsFormatException);
    // A flow mapping cannot be replaced safely with this block-splicing tool.
    // The candidate must be parsed before the CLI can rename it into place.
    expect(
        () => applyCoreProfile(
            '{name: host, sing_box_flutter: {core: {}}}\n', profile),
        throwsFormatException);
  });

  for (final entry in {
    'ordinary pubspec': host,
    ...aliasedHosts,
    'unsafe flow splice': '{name: host, sing_box_flutter: {core: {}}}\n',
  }.entries) {
    test('real profile CLI handles ${entry.key} in a disposable host',
        () async {
      final fixture = Directory.systemTemp.createTempSync('core-profile-cli-');
      addTearDown(() => fixture.deleteSync(recursive: true));
      final actual = File('pubspec.yaml');
      final actualBytes = actual.readAsBytesSync();
      final original = [0xef, 0xbb, 0xbf, ...utf8.encode(entry.value)];
      final pubspec = File('${fixture.path}/pubspec.yaml')
        ..writeAsBytesSync(original);
      final profiles = Directory('${fixture.path}/core-profiles')..createSync();
      final profile = File('core-profiles/wgc-minimal.yaml').readAsStringSync();
      File('${profiles.path}/wgc-minimal.yaml').writeAsStringSync(profile);
      final result = await runProfileFixture(fixture);
      final succeeds = entry.key == 'ordinary pubspec';
      expect(result.exitCode, succeeds ? 0 : 64,
          reason: '${result.stdout}\n${result.stderr}');
      if (succeeds) {
        expect(withoutSelection(pubspec.readAsStringSync()),
            withoutSelection(entry.value));
        expect(loadYaml(pubspec.readAsStringSync())['sing_box_flutter'],
            loadYaml(profile)['sing_box_flutter']);
      } else {
        expect(result.stderr, contains('Profile not applied:'));
        expect(result.stderr, isNot(contains('Unhandled exception')));
        expect(pubspec.readAsBytesSync(), original);
      }
      expect(
          fixture
              .listSync()
              .whereType<File>()
              .where((file) => file.path.contains('.pubspec.profile.')),
          isEmpty);
      expect(actual.readAsBytesSync(), actualBytes);
    });
  }

  test('full mobile profile includes every endpoint and management API', () {
    final full =
        loadYaml(File('core-profiles/singbox-full.yaml').readAsStringSync())[
            'sing_box_flutter']['core'];
    expect(
        full['endpoints'],
        containsAll([
          'wgc',
          'wireguard',
          'tailscale',
          'openconnect',
          'openvpn-client',
          'openvpn-server'
        ]));
    expect(full['api'], containsAll(['clash_api', 'v2ray_api']));
    expect(full['outbounds'],
        containsAll(['vless', 'shadowsocks', 'hysteria2', 'tuic']));
    expect(full['inbounds'], isNot(contains('tproxy')));
    expect(full['outbounds'], isNot(contains('naive')));
  });
}
