import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

import '../bin/sing_box_flutter.dart' as cli;

void main() {
  late Directory fixture;
  late Directory caller;
  late Directory home;
  late Map<String, String> environment;

  setUp(() {
    fixture = Directory.systemTemp.createTempSync('cli-path-forwarding-');
    caller = Directory.fromUri(fixture.uri.resolve('caller/'))..createSync();
    home = Directory.fromUri(fixture.uri.resolve('home/'))..createSync();
    environment = {
      'HOME': Platform.isWindows ? 'unused-unix-home' : home.path,
      'USERPROFILE': Platform.isWindows ? home.path : 'unused-windows-home',
    };
  });

  tearDown(() => fixture.deleteSync(recursive: true));

  for (final option in ['--wgc-config', '--from-dir']) {
    for (final equalsForm in [false, true]) {
      final form = equalsForm ? 'equals' : 'separate';
      for (final homeRelative in [false, true]) {
        test('$option $form preserves ${homeRelative ? 'home' : 'caller'} path',
            () {
          final base = homeRelative ? home : caller;
          final target = option == '--wgc-config'
              ? (File.fromUri(base.uri.resolve('wgc.core.json'))
                    ..writeAsStringSync('{}'))
                  .path
              : (Directory.fromUri(base.uri.resolve('artifacts'))..createSync())
                  .path;
          final name = option == '--wgc-config' ? 'wgc.core.json' : 'artifacts';
          final input = '${homeRelative ? '~/' : ''}$name';
          final arguments = [
            '--target',
            'apple',
            if (equalsForm) '$option=$input' else ...[option, input],
          ];
          final original = List<String>.of(arguments);

          final forwarded = cli.forwardPathArguments(
            arguments,
            callerDirectory: caller,
            environment: environment,
          );

          expect(forwarded, [
            '--target',
            'apple',
            if (equalsForm) '$option=$target' else ...[option, target],
          ]);
          expect(arguments, original);
          final forwardedPath = equalsForm
              ? forwarded.last.substring(option.length + 1)
              : forwarded.last;
          expect(
              FileSystemEntity.typeSync(forwardedPath),
              option == '--wgc-config'
                  ? FileSystemEntityType.file
                  : FileSystemEntityType.directory);
        });
      }
    }
  }

  for (final option in ['--wgc-config', '--from-dir']) {
    for (final equalsForm in [false, true]) {
      for (final origin in ['caller', 'home', 'absolute']) {
        test(
            '$option preserves symlink/.. semantics ($origin, equals=$equalsForm)',
            () {
          final base = origin == 'home' ? home : caller;
          final external = Directory('${fixture.path}/external')..createSync();
          final child = Directory('${external.path}/child')..createSync();
          Link('${base.path}/link').createSync(child.path);
          final name = option == '--wgc-config' ? 'wgc.core.json' : 'artifacts';
          final target = '${external.path}/$name';
          if (option == '--wgc-config') {
            File(target).writeAsStringSync('selected manifest');
            File('${base.path}/$name').writeAsStringSync('wrong manifest');
          } else {
            Directory(target).createSync();
            Directory('${base.path}/$name').createSync();
          }
          final prefix = switch (origin) {
            'home' => '~/',
            'absolute' => '${base.path}/',
            _ => '',
          };
          final input = '${prefix}link/../$name';
          final forwarded = cli.forwardPathArguments(
            equalsForm ? ['$option=$input'] : [option, input],
            callerDirectory: caller,
            environment: environment,
          );
          final path = equalsForm
              ? forwarded.last.substring(option.length + 1)
              : forwarded.last;
          expect(FileSystemEntity.identicalSync(path, target), isTrue);
          expect(path, contains('link/../'));
        }, skip: Platform.isWindows);
      }
    }
  }

  test('preserves absolute paths and unrelated or incomplete options', () {
    final absolute = File.fromUri(home.uri.resolve('absolute.json')).path;
    final arguments = [
      '--wgc-config=$absolute',
      '--from-dir',
      home.path,
      '--release=~/unchanged',
      '--wgc-config',
    ];
    expect(
      cli.forwardPathArguments(arguments,
          callerDirectory: caller, environment: environment),
      arguments,
    );
  });

  test('expands bare home and preserves Python user expansion', () {
    expect(
      cli.forwardPathArguments(
          ['--from-dir=~', '--wgc-config', '~other/a.json'],
          callerDirectory: caller, environment: environment),
      ['--from-dir=${home.path}', '--wgc-config', '~other/a.json'],
    );
  });

  test('leaves home expansion to Python when home is unavailable', () {
    expect(
      cli.forwardPathArguments(
          ['--wgc-config=~/wgc.core.json', '--from-dir', '~'],
          callerDirectory: caller,
          environment: {}),
      ['--wgc-config=~/wgc.core.json', '--from-dir', '~'],
    );
  });

  test('Windows falls back to HOMEDRIVE and HOMEPATH', () {
    expect(
      cli.forwardPathArguments(['--from-dir=~\\artifacts'],
          callerDirectory: caller,
          environment: {'HOMEDRIVE': 'C:', 'HOMEPATH': r'\Users\tester'}),
      [r'--from-dir=C:\Users\tester\artifacts'],
    );
  }, skip: !Platform.isWindows);
}
