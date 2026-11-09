import 'dart:convert';
import 'dart:io';

import 'package:crypto/crypto.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter/src/core_build_config.dart';

void main() {
  late Directory fixture;
  setUp(() => fixture = Directory.systemTemp.createTempSync('core-config-'));
  tearDown(() => fixture.deleteSync(recursive: true));

  test('omitted core preserves legacy behavior; empty core gets defaults', () {
    final pubspec = File('${fixture.path}/pubspec.yaml');
    for (final yaml in ['name: host', 'sing_box_flutter: {}']) {
      pubspec.writeAsStringSync(yaml);
      expect(readCoreBuildSnapshot(pubspec), isNull);
    }
    pubspec.writeAsStringSync('sing_box_flutter:\n  core: {}\n');
    final selection = readCoreBuildSnapshot(pubspec)!.features;
    expect(selection['inbounds'], ['tun']);
    expect(selection['outbounds'], ['direct']);
    expect(selection['dns'], ['local', 'tcp', 'udp']);
    expect(selection['endpoints'], isEmpty);
  });

  test('only an omitted DNS category gets local resolver defaults', () {
    expect(parseCoreSelection({})['dns'], ['local', 'tcp', 'udp']);
    expect(parseCoreSelection({'dns': []})['dns'], isEmpty);
    expect(
        parseCoreSelection({
          'dns': ['udp']
        })['dns'],
        ['udp']);
  });

  test('snapshot hashes exact source bytes including BOM and CRLF', () {
    final pubspec = File('${fixture.path}/pubspec.yaml');
    const text = 'name: host\nsing_box_flutter:\n  core: {}\n';
    final plain = utf8.encode(text);
    final withBom = [
      0xef,
      0xbb,
      0xbf,
      ...utf8.encode(text.replaceAll('\n', '\r\n'))
    ];
    pubspec.writeAsBytesSync(plain);
    final first = readCoreBuildSnapshot(pubspec)!;
    pubspec.writeAsBytesSync(withBom);
    final second = readCoreBuildSnapshot(pubspec)!;
    expect(first.features, second.features);
    expect(first.pubspecSha256, sha256.convert(plain).toString());
    expect(second.pubspecSha256, sha256.convert(withBom).toString());
    expect(first.pubspecSha256, isNot(second.pubspecSha256));
    expect(second.pubspec.path, pubspec.resolveSymbolicLinksSync());
  });

  test(
      'private requests preserve captured source across overlapping invocations',
      () {
    final pubspec = File('${fixture.path}/pubspec.yaml');
    final firstBytes =
        utf8.encode('sing_box_flutter:\n  core:\n    endpoints: [wgc]\n');
    pubspec.writeAsBytesSync(firstBytes);
    final first = readCoreBuildSnapshot(pubspec)!;
    final request = writeCoreBuildRequest(first);
    final originalRequest = request.readAsStringSync();
    pubspec.writeAsStringSync(
        'sing_box_flutter:\n  core:\n    endpoints: [wireguard]\n');
    final second = readCoreBuildSnapshot(pubspec)!;
    final other = writeCoreBuildRequest(second);
    expect(request.path, isNot(other.path));
    expect(request.parent.path, isNot(other.parent.path));
    expect(request.readAsStringSync(), originalRequest);
    expect(jsonDecode(originalRequest), {
      'schema': 1,
      'pubspec': {
        'path': pubspec.resolveSymbolicLinksSync(),
        'sha256': sha256.convert(firstBytes).toString(),
      },
      'features': first.features,
    });
    expect(jsonDecode(other.readAsStringSync())['features']['endpoints'],
        ['wireguard']);
    expect(() => first.features['dns']!.add('https'), throwsUnsupportedError);
    expect(() => first.features['dns'] = [], throwsUnsupportedError);
  });

  test('private requests never traverse a symlinked host cache', () {
    for (final nested in [false, true]) {
      final host = Directory('${fixture.path}/host-$nested')..createSync();
      final outside = Directory('${fixture.path}/outside-$nested')
        ..createSync();
      final pubspec = File('${host.path}/pubspec.yaml')
        ..writeAsStringSync('sing_box_flutter:\n  core: {}\n');
      final cache = nested
          ? (Directory('${host.path}/.dart_tool')..createSync()).path
          : host.path;
      Link('$cache/${nested ? 'sing_box_flutter' : '.dart_tool'}')
          .createSync(outside.path);
      final snapshot = readCoreBuildSnapshot(pubspec)!;
      expect(() => writeCoreBuildRequest(snapshot),
          throwsA(isA<FileSystemException>()));
      expect(outside.listSync(), isEmpty);
    }
  }, skip: Platform.isWindows);

  test('rejects wrong types, unknown names, duplicates and null core', () {
    for (final value in [
      null,
      [],
      {'unknown': []},
      {'dns': null},
      {'dns': 'udp'},
      {
        'dns': [1]
      },
      {
        'dns': ['udp', 'udp']
      },
      {
        'dns': ['udpp']
      },
    ]) {
      expect(() => parseCoreSelection(value), throwsFormatException);
    }
    final pubspec = File('${fixture.path}/pubspec.yaml');
    for (final yaml in [
      'sing_box_flutter:\n  core: null',
      'sing_box_flutter:\n  coree: {}',
      'sing_box_flutter:\n  core:\n    dns: []\n    dns: [udp]',
    ]) {
      pubspec.writeAsStringSync(yaml);
      expect(() => readCoreBuildSnapshot(pubspec), throwsFormatException);
    }
  });

  test('request features preserve explicit empty and have stable input order',
      () {
    final pubspec = File('${fixture.path}/pubspec.yaml');
    final features = <String>[];
    for (final source in [
      'sing_box_flutter:\n  core:\n    dns: []\n    endpoints: [wireguard, wgc]\n',
      'sing_box_flutter:\n  core:\n    endpoints: [wgc, wireguard]\n    dns: []\n',
    ]) {
      pubspec.writeAsStringSync(source);
      final request = writeCoreBuildRequest(readCoreBuildSnapshot(pubspec)!);
      final document = jsonDecode(request.readAsStringSync());
      expect(document['features']['dns'], isEmpty);
      features.add(jsonEncode(document['features']));
      expect(request.parent.listSync(), hasLength(1));
    }
    expect(features.first, features.last);
  });

  test('nearest parent pubspec wins over package ancestors', () {
    File('${fixture.path}/pubspec.yaml').writeAsStringSync('name: ancestor');
    final host = Directory('${fixture.path}/host')..createSync();
    final pubspec = File('${host.path}/pubspec.yaml')
      ..writeAsStringSync('name: host');
    final nested = Directory('${host.path}/lib/deep')
      ..createSync(recursive: true);
    expect(findHostPubspec(nested)?.path, pubspec.path);
  });
}
