import 'dart:io';
import 'dart:isolate';

import 'package:path/path.dart' as p;
import 'package:sing_box_flutter/src/core_build_config.dart';

/// Builds a host-selected native core or manages legacy standard-core artifacts.
Future<void> main(List<String> arguments) async {
  if (arguments.isEmpty || arguments.first == '--help') {
    stdout.writeln('''Native core setup for sing_box_flutter.

Build from source (0.3.0):
  dart run sing_box_flutter build --target android
  dart run sing_box_flutter build --target apple

Add sing_box_flutter.core to your host pubspec.yaml to select features.
Run build from that app; the pinned Tailscale Go toolchain bootstraps automatically.
Use --config <pubspec path> or --project-dir <host root> to select another host.
Requires Python 3.11+ and Git, plus JDK 17/Android SDK/NDK for Android or
macOS/Xcode for Apple. No global Go or separate runtime Go installation is needed.
0.3.0 releases contain source/build scripts, with no precompiled native SDK assets.

Legacy standard core (hosts without sing_box_flutter.core):
  dart run sing_box_flutter install --target android --release v0.2.0
  dart run sing_box_flutter install --target apple --release v0.2.0

install: download and verify historical native release artifacts (Python 3.11+).
Without a feature selection, build retains the legacy standard source-build path.
Selected cores require build; install cannot satisfy a host feature selection.
Pass --help after a subcommand for its options.''');
    return;
  }
  var script = switch (arguments.first) {
    'install' => 'install_libbox.py',
    'build' => 'build_libbox.py',
    _ => null,
  };
  if (script == null) {
    stderr.writeln('Unknown command. Use: dart run sing_box_flutter --help');
    exitCode = 64;
    return;
  }
  final library = await Isolate.resolvePackageUri(
    Uri.parse('package:sing_box_flutter/sing_box_flutter.dart'),
  );
  if (library == null || library.scheme != 'file') {
    stderr.writeln('Cannot locate the sing_box_flutter package directory.');
    exitCode = 66;
    return;
  }
  final root = File.fromUri(library).parent.parent;
  late List<String> forwarded;
  File? request;
  try {
    final invocation = resolveHostArguments(arguments.skip(1).toList(),
        callerDirectory: Directory.current, environment: Platform.environment);
    forwarded = invocation.arguments;
    // Help must work even if the host selection is incomplete or unsupported.
    if (!forwarded.contains('--help') && !forwarded.contains('-h')) {
      final snapshot = invocation.pubspec == null
          ? null
          : readCoreBuildSnapshot(invocation.pubspec!);
      if (snapshot != null) {
        if (arguments.first == 'install') {
          throw const FormatException(
              'Custom core selections have no matching release artifacts. '
              'Use dart run sing_box_flutter build instead.');
        }
        if (forwarded.any((argument) =>
            argument == '--features-file' ||
            argument.startsWith('--features-file='))) {
          throw const FormatException(
              '--features-file is managed by the CLI. Select the host with --config instead.');
        }
        script = 'build_core.py';
        request = writeCoreBuildRequest(snapshot);
        forwarded = [
          '--features-file',
          request.path,
          '--project-dir',
          snapshot.pubspec.parent.path,
          ...forwarded,
        ];
      }
    }
    final executable = File.fromUri(root.uri.resolve('tool/$script'));
    if (!executable.existsSync()) {
      stderr.writeln('Native setup script is missing from the package.');
      exitCode = 66;
      return;
    }
    final python = await _findPython();
    if (python == null) {
      stderr.writeln('Python 3.11+ must be available on PATH.');
      exitCode = 69;
      return;
    }
    final process = await Process.start(
      python,
      [executable.path, ...forwarded],
      workingDirectory: root.path,
      mode: ProcessStartMode.inheritStdio,
    );
    exitCode = await process.exitCode;
  } on FormatException catch (error) {
    stderr.writeln(error.message);
    exitCode = 64;
  } on FileSystemException catch (error) {
    stderr.writeln('Cannot read or write host core configuration: $error');
    exitCode = 66;
  } on ProcessException {
    stderr.writeln('Could not start the native setup tool.');
    exitCode = 69;
  } finally {
    if (request != null && request.parent.existsSync()) {
      request.parent.deleteSync(recursive: true);
    }
  }
}

/// Resolves CLI-owned host selectors while preserving legacy tool arguments.
({File? pubspec, List<String> arguments}) resolveHostArguments(
  List<String> arguments, {
  required Directory callerDirectory,
  required Map<String, String> environment,
}) {
  final forwarded = forwardPathArguments(arguments,
      callerDirectory: callerDirectory, environment: environment);
  String? config;
  String? project;
  final remaining = <String>[];
  for (var index = 0; index < forwarded.length; index++) {
    final argument = forwarded[index];
    final option = argument.split('=').first;
    if (option != '--config' && option != '--project-dir') {
      remaining.add(argument);
      continue;
    }
    final String value;
    if (argument.contains('=')) {
      value = argument.substring(option.length + 1);
    } else {
      if (++index >= forwarded.length || forwarded[index].startsWith('--')) {
        throw FormatException('$option requires a path');
      }
      value = forwarded[index];
    }
    if (value.isEmpty) throw FormatException('$option requires a path');
    if (option == '--config') {
      if (config != null) throw const FormatException('Duplicate --config');
      config = value;
    } else {
      if (project != null) {
        throw const FormatException('Duplicate --project-dir');
      }
      project = value;
    }
  }
  if (config != null && project != null) {
    throw const FormatException('Use only one of --config and --project-dir');
  }
  final File? pubspec;
  if (config != null) {
    pubspec = File(config);
  } else if (project != null) {
    pubspec = File(p.join(project, 'pubspec.yaml'));
  } else {
    pubspec = findHostPubspec(callerDirectory);
  }
  if (pubspec != null && !pubspec.existsSync()) {
    throw FormatException('Host pubspec does not exist: ${pubspec.path}');
  }
  final canonical =
      pubspec == null ? null : File(pubspec.resolveSymbolicLinksSync());
  if (canonical != null && p.basename(canonical.path) != 'pubspec.yaml') {
    throw FormatException(
        '--config must resolve to the host pubspec.yaml, not ${canonical.path}');
  }
  return (pubspec: canonical, arguments: remaining);
}

/// Keeps path options valid when the subprocess runs in the package directory.
List<String> forwardPathArguments(
  List<String> arguments, {
  required Directory callerDirectory,
  required Map<String, String> environment,
  p.Context? pathContext,
}) {
  final paths =
      pathContext ?? p.Context(current: callerDirectory.absolute.path);
  final windows = paths.style == p.Style.windows;
  final String? home;
  if (windows) {
    home = environment['USERPROFILE'] ??
        (environment.containsKey('HOMEDRIVE') &&
                environment.containsKey('HOMEPATH')
            ? '${environment['HOMEDRIVE']}${environment['HOMEPATH']}'
            : null);
  } else {
    home = environment['HOME'];
  }

  String resolve(String path) {
    if (path == '~' ||
        path.startsWith('~/') ||
        (windows && path.startsWith('~\\'))) {
      // Leave unresolved home paths for Python's expanduser fallback.
      if (home == null || home.isEmpty) return path;
      if (path == '~') {
        path = home;
      } else {
        // Expand the prefix literally: extra separators belong beneath home.
        final prefix =
            home.replaceFirst(RegExp(windows ? r'[/\\]+$' : r'/+$'), '');
        path = '$prefix${path.substring(1)}';
      }
    } else if (path.startsWith('~')) {
      // Python also supports ~user on Unix; do not anchor it to the caller.
      return path;
    }
    // join anchors Windows rooted paths to the caller's drive or UNC share
    // without normalizing dot segments before the filesystem follows symlinks.
    return paths.join(paths.current, path);
  }

  final forwarded = List<String>.of(arguments);
  for (var index = 0; index < forwarded.length; index++) {
    for (final option in [
      '--wgc-config',
      '--from-dir',
      '--config',
      '--project-dir',
      '--work-dir'
    ]) {
      if (forwarded[index] == option && index + 1 < forwarded.length) {
        forwarded[++index] = resolve(forwarded[index]);
        break;
      } else if (forwarded[index].startsWith('$option=')) {
        forwarded[index] =
            '$option=${resolve(forwarded[index].substring(option.length + 1))}';
        break;
      }
    }
  }
  return forwarded;
}

Future<String?> _findPython() async {
  for (final candidate in [
    'python3',
    'python3.14',
    'python3.13',
    'python3.12',
    'python3.11',
    'python',
  ]) {
    try {
      final result = await Process.run(candidate, [
        '-I',
        '-c',
        'import sys; sys.exit(sys.version_info < (3, 11))',
      ]);
      if (result.exitCode == 0) return candidate;
    } on ProcessException {
      // Try the next conventional interpreter name without modifying PATH.
    }
  }
  return null;
}
