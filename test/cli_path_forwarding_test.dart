import 'dart:convert';
import 'dart:io';

import 'package:crypto/crypto.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:path/path.dart' as p;

import '../bin/sing_box_flutter.dart' as cli;

Future<ProcessResult> _runBuild(
    Directory host, Map<String, String> environment) async {
  final root = Directory.current.absolute;
  final packageConfig = File('${root.path}/.dart_tool/package_config.json');
  final packages =
      jsonDecode(packageConfig.readAsStringSync())['packages'] as List;
  final flutter = Directory.fromUri(packageConfig.uri.resolve(
      packages.singleWhere((entry) => entry['name'] == 'flutter')['rootUri']
          as String));
  final dart = File(p.join(flutter.parent.parent.path, 'bin', 'cache',
      'dart-sdk', 'bin', Platform.isWindows ? 'dart.exe' : 'dart'));
  return Process.run(
      dart.path,
      [
        '--packages=${root.path}/.dart_tool/package_config.json',
        '${root.path}/bin/sing_box_flutter.dart',
        'build',
        '--project-dir',
        host.path,
        '--target',
        'android',
      ],
      workingDirectory: host.path,
      environment: environment);
}

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

  test('anchors a relative native work directory to the caller', () {
    for (final arguments in [
      ['--work-dir', 'native-work'],
      ['--work-dir=native-work'],
    ]) {
      final forwarded = cli.forwardPathArguments(arguments,
          callerDirectory: caller, environment: environment);
      expect(forwarded.last, endsWith(p.join(caller.path, 'native-work')));
    }
  });

  for (final option in ['--wgc-config', '--from-dir']) {
    for (final equalsForm in [false, true]) {
      test('repeated home separators ($option, equals=$equalsForm)', () {
        final base = '${fixture.path}/home';
        final external = Directory('${fixture.path}/external')..createSync();
        final child = Directory('${external.path}/child')..createSync();
        Link('$base/link').createSync(child.path);
        final name = option == '--wgc-config' ? 'wgc.core.json' : 'artifacts';
        for (final parent in [base, external.path]) {
          if (option == '--wgc-config') {
            File('$parent/$name').writeAsStringSync('{}');
          } else {
            Directory('$parent/$name').createSync();
          }
        }
        final cases = {
          '~//$name': '$base/$name',
          '~///$name': '$base/$name',
          '~//link/../$name': '${external.path}/$name',
        };
        for (final homePath in [base, '$base/', '$base///']) {
          for (final entry in cases.entries) {
            final forwarded = cli.forwardPathArguments(
              equalsForm ? ['$option=${entry.key}'] : [option, entry.key],
              callerDirectory: caller,
              environment: {'HOME': homePath},
            );
            final path = equalsForm
                ? forwarded.last.substring(option.length + 1)
                : forwarded.last;
            expect(path, startsWith('$base//'));
            expect(FileSystemEntity.identicalSync(path, entry.value), isTrue);
          }
        }
      }, skip: Platform.isWindows);
    }
  }

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

  for (final current in [r'D:\projects\caller', r'\\server\share\caller']) {
    for (final option in ['--wgc-config', '--from-dir']) {
      for (final equalsForm in [false, true]) {
        test('Windows rooted paths ($current, $option, equals=$equalsForm)',
            () {
          final paths = p.Context(style: p.Style.windows, current: current);
          final root = current.startsWith(r'\\') ? r'\\server\share' : 'D:';
          final cases = {
            r'\artifacts': '$root\\artifacts',
            '/artifacts': '$root\\artifacts',
            r'\link\..\artifacts': '$root\\link\\..\\artifacts',
            '/link/../artifacts': '$root\\link/../artifacts',
            r'link\..\wgc.core.json': '$current\\link\\..\\wgc.core.json',
            r'C:\cache\link\..\artifacts': r'C:\cache\link\..\artifacts',
            r'\\other\share\link\..\artifacts':
                r'\\other\share\link\..\artifacts',
            r'~\link\..\artifacts': r'C:\Users\tester\link\..\artifacts',
            r'~\\artifacts': r'C:\Users\tester\\artifacts',
            '~//artifacts': r'C:\Users\tester//artifacts',
            r'~\/artifacts': r'C:\Users\tester\/artifacts',
            r'~\\\other\share\artifacts':
                r'C:\Users\tester\\\other\share\artifacts',
          };
          for (final entry in cases.entries) {
            final forwarded = cli.forwardPathArguments(
              equalsForm ? ['$option=${entry.key}'] : [option, entry.key],
              callerDirectory: caller,
              environment: {'USERPROFILE': r'C:\Users\tester'},
              pathContext: paths,
            );
            expect(forwarded,
                equalsForm ? ['$option=${entry.value}'] : [option, entry.value],
                reason: entry.key);
          }
        });
      }
    }
  }

  test('host selection finds nearest pubspec below host root', () {
    File('${caller.path}/pubspec.yaml').writeAsStringSync('name: host');
    final nested = Directory('${caller.path}/lib/deep')
      ..createSync(recursive: true);
    final invocation = cli.resolveHostArguments(
      ['--target', 'android', '--wgc-config', 'wgc.json'],
      callerDirectory: nested,
      environment: environment,
    );
    expect(
        FileSystemEntity.identicalSync(
            invocation.pubspec!.path, p.join(caller.path, 'pubspec.yaml')),
        isTrue);
    expect(invocation.arguments, [
      '--target',
      'android',
      '--wgc-config',
      p.join(nested.path, 'wgc.json')
    ]);
  });

  test('explicit host selectors are resolved and removed before forwarding',
      () {
    final host = Directory('${caller.path}/host')..createSync();
    File('${host.path}/pubspec.yaml').writeAsStringSync('name: host');
    for (final selector in [
      ['--config', 'host/pubspec.yaml'],
      ['--config=host/pubspec.yaml'],
      ['--project-dir', 'host'],
      ['--project-dir=host'],
    ]) {
      final invocation = cli.resolveHostArguments(
          [...selector, '--target=apple', '--release', 'v0.2.0'],
          callerDirectory: caller, environment: environment);
      expect(
          FileSystemEntity.identicalSync(
              invocation.pubspec!.path, p.join(host.path, 'pubspec.yaml')),
          isTrue);
      expect(invocation.arguments, ['--target=apple', '--release', 'v0.2.0']);
    }
    for (final invalid in [
      ['--config'],
      ['--config='],
      ['--project-dir', '--target'],
      ['--config', 'missing'],
      ['--config', 'host/pubspec.yaml', '--project-dir', 'host'],
      ['--config=host/pubspec.yaml', '--config=host/pubspec.yaml'],
    ]) {
      expect(
          () => cli.resolveHostArguments(invalid,
              callerDirectory: caller, environment: environment),
          throwsFormatException);
    }
  });

  test(
      'alternate config filenames fail clearly instead of selecting another host',
      () {
    File('${caller.path}/pubspec.yaml').writeAsStringSync('name: host');
    File('${caller.path}/selected.yaml').writeAsStringSync('name: other');
    expect(
      () => cli.resolveHostArguments(['--config', 'selected.yaml'],
          callerDirectory: caller, environment: environment),
      throwsA(isA<FormatException>().having(
          (error) => error.message, 'message', contains('host pubspec.yaml'))),
    );
  });

  test('config symlinks select the canonical pubspec and its real host root',
      () {
    final host = Directory('${fixture.path}/real-host')..createSync();
    final pubspec = File('${host.path}/pubspec.yaml')
      ..writeAsStringSync('name: host');
    Link('${caller.path}/selected.yaml').createSync(pubspec.path);
    final invocation = cli.resolveHostArguments(['--config=selected.yaml'],
        callerDirectory: caller, environment: environment);
    expect(invocation.pubspec!.path, pubspec.resolveSymbolicLinksSync());
    expect(invocation.pubspec!.parent.path, host.resolveSymbolicLinksSync());
  }, skip: Platform.isWindows);

  for (final toolExit in [0, 73]) {
    test(
        'real CLI keeps its private snapshot through Python discovery (exit $toolExit)',
        () async {
      final original = utf8.encode(
          'name: host\nsing_box_flutter:\n  core:\n    endpoints: [wgc]\n');
      final pubspec = File('${caller.path}/pubspec.yaml')
        ..writeAsBytesSync(original);
      final capture = File('${fixture.path}/capture.json');
      final bin = Directory('${fixture.path}/bin')..createSync();
      final shim = File('${bin.path}/python3')
        ..writeAsStringSync(r'''#!/usr/bin/python3 -I
import json
import os
from pathlib import Path
import sys

host = Path(os.environ['MOCK_HOST'])
if sys.argv[1:3] == ['-I', '-c']:
    requests = list((host / '.dart_tool/sing_box_flutter').glob('request-*/request.json'))
    assert len(requests) == 1
    (host / 'pubspec.yaml').write_text('name: host\nsing_box_flutter:\n  core:\n    endpoints: [wireguard]\n')
    raise SystemExit(0)

assert Path(sys.argv[1]).name == 'build_core.py'
request = Path(sys.argv[sys.argv.index('--features-file') + 1])
project = sys.argv[sys.argv.index('--project-dir') + 1]
Path(os.environ['MOCK_CAPTURE']).write_text(json.dumps({
    'requestPath': str(request),
    'request': json.loads(request.read_text()),
    'project': project,
}))
raise SystemExit(int(os.environ['MOCK_TOOL_EXIT']))
''');
      Process.runSync('/bin/chmod', ['+x', shim.path]);
      final result = await _runBuild(caller, {
        'PATH': bin.path,
        'MOCK_HOST': caller.resolveSymbolicLinksSync(),
        'MOCK_CAPTURE': capture.path,
        'MOCK_TOOL_EXIT': '$toolExit',
      });
      expect(result.exitCode, toolExit,
          reason: '${result.stdout}\n${result.stderr}');
      final observed = jsonDecode(capture.readAsStringSync()) as Map;
      final request = observed['request'] as Map;
      expect(request['schema'], 1);
      expect(request['pubspec'], {
        'path': pubspec.resolveSymbolicLinksSync(),
        'sha256': sha256.convert(original).toString(),
      });
      expect(request['features']['endpoints'], ['wgc']);
      expect(request['features']['dns'], ['local', 'tcp', 'udp']);
      expect(pubspec.readAsStringSync(), contains('endpoints: [wireguard]'));
      expect(observed['project'], caller.resolveSymbolicLinksSync());
      final requestFile = File(observed['requestPath'] as String);
      expect(requestFile.existsSync(), isFalse);
      expect(requestFile.parent.existsSync(), isFalse);
    }, skip: Platform.isWindows || !File('/usr/bin/python3').existsSync());
  }

  test('real CLI removes its request when no interpreter can start', () async {
    File('${caller.path}/pubspec.yaml')
        .writeAsStringSync('sing_box_flutter:\n  core: {}\n');
    final bin = Directory('${fixture.path}/bin')..createSync();
    for (final name in [
      'python3',
      'python3.14',
      'python3.13',
      'python3.12',
      'python3.11',
      'python'
    ]) {
      final shim = File('${bin.path}/$name')
        ..writeAsStringSync('#!/bin/sh\nexit 1\n');
      Process.runSync('/bin/chmod', ['+x', shim.path]);
    }
    final result = await _runBuild(caller, {'PATH': bin.path});
    expect(result.exitCode, 69, reason: '${result.stdout}\n${result.stderr}');
    expect(result.stderr, contains('Python 3.11+'));
    expect(Directory('${caller.path}/.dart_tool/sing_box_flutter').listSync(),
        isEmpty);
  }, skip: Platform.isWindows);

  test('Windows falls back to HOMEDRIVE and HOMEPATH', () {
    expect(
      cli.forwardPathArguments(['--from-dir=~\\artifacts'],
          callerDirectory: caller,
          environment: {'HOMEDRIVE': 'C:', 'HOMEPATH': r'\Users\tester'},
          pathContext:
              p.Context(style: p.Style.windows, current: r'D:\caller')),
      [r'--from-dir=C:\Users\tester\artifacts'],
    );
  });
}
