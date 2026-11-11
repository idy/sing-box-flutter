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
  dart run sing_box_flutter build --target macos
  dart run sing_box_flutter build --target windows --windows-arch amd64
  dart run sing_box_flutter build --target linux --arch amd64

Windows supports amd64 and arm64, using native Windows or macOS/Linux cross builds.
The helper embeds the pinned signed Wintun driver and retains its official license.
With a core block, --target all builds Android + Apple + macOS.
Legacy --target all builds Android + Apple. Linux and Windows require explicit targets.

Add sing_box_flutter.core to your host pubspec.yaml to select features.
Select macOS with sing_box_flutter.macos.mode: ne (default) or user_tun.
Run build from that app; the pinned Tailscale Go toolchain bootstraps automatically.
Use --config <pubspec path> or --project-dir <host root> to select another host.
Requires Python 3.11+ and Git, plus JDK 17/Android SDK/NDK for Android or
macOS/Xcode for Apple. No global Go or separate runtime Go installation is needed.
0.3.0 releases contain source/build scripts, with no precompiled native SDK assets.

Legacy iOS/Android standard core (hosts without sing_box_flutter.core):
  dart run sing_box_flutter install --target android --release v0.2.0
  dart run sing_box_flutter install --target apple --release v0.2.0

install: download and verify historical native release artifacts (Python 3.11+).
Without a core block, mobile targets retain the legacy standard build/install path.
A macos-only block builds source defaults for --target macos; run mobile targets
separately. Mixed legacy/source selections require an explicit platform target.
Linux requires a feature selection and source build; --arch defaults to the host
architecture (amd64 or arm64). Linux cross builds work from macOS without an SDK.
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
    final target = requestedCoreTarget(forwarded);
    final windows = target == 'windows';
    final linux = target == 'linux';
    if ((windows || linux) && arguments.first == 'build') {
      script = 'build_core.py';
    }
    // Help must work even if the host selection is incomplete or unsupported.
    if (!forwarded.contains('--help') && !forwarded.contains('-h')) {
      if (linux && arguments.first == 'install') {
        throw const FormatException(
            'Linux has no legacy release installer. Select sing_box_flutter.core '
            'in the host pubspec.yaml and use dart run sing_box_flutter build --target linux.');
      }
      final snapshot = invocation.pubspec == null
          ? null
          : readCoreBuildSnapshot(invocation.pubspec!);
      if (windows && (snapshot == null || !snapshot.hasCoreSelection)) {
        throw const FormatException(
            'Windows requires a host sing_box_flutter.core selection. '
            'Add sing_box_flutter: {core: {}} to your pubspec.yaml, then '
            'run dart run sing_box_flutter build --target windows.');
      }
      if (linux && (snapshot == null || !snapshot.hasCoreSelection)) {
        throw const FormatException(
            'Linux source builds require sing_box_flutter.core in the host pubspec.yaml. '
            'Add a feature selection, then run dart run sing_box_flutter build --target linux.');
      }
      final selected = snapshot != null &&
          (snapshot.hasCoreSelection || _macosOnlySourceTarget(forwarded));
      if (snapshot != null && selected) {
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
      ['-X', 'utf8', executable.path, ...forwarded],
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

/// Matches argparse's final --target value, including its equals form.
String? requestedCoreTarget(List<String> arguments) {
  String? target;
  for (var index = 0; index < arguments.length; index++) {
    if (arguments[index].startsWith('--target=')) {
      target = arguments[index].substring('--target='.length);
    } else if (arguments[index] == '--target' && index + 1 < arguments.length) {
      target = arguments[++index];
    }
  }
  return target;
}

/// A backend-only selection combines legacy mobile and selected macOS cores.
/// Require one explicit target so `all` cannot change mobile capabilities or
/// report success while omitting the configured macOS core.
bool _macosOnlySourceTarget(List<String> arguments) {
  String? target;
  for (var index = 0; index < arguments.length; index++) {
    final argument = arguments[index];
    final option = argument.split('=').first;
    if (option != '--target') {
      // argparse accepts unique option abbreviations; allowing them here could
      // override the platform used to choose the native setup script.
      if (option.length > 2 && '--target'.startsWith(option)) {
        throw const FormatException('Use the full --target option.');
      }
      continue;
    }
    if (target != null) throw const FormatException('Duplicate --target');
    if (argument.contains('=')) {
      target = argument.substring(option.length + 1);
    } else {
      if (++index >= arguments.length) {
        throw const FormatException('--target requires a platform');
      }
      target = arguments[index];
    }
    if (!['android', 'apple', 'macos', 'all'].contains(target)) {
      throw const FormatException(
          '--target must be android, apple, macos or all');
    }
  }
  if (target == null || target == 'all') {
    throw const FormatException(
        'A macos-only configuration mixes legacy mobile and source macOS cores. '
        'Use an explicit --target android or --target apple for legacy setup, '
        'and build --target macos separately.');
  }
  return target == 'macos';
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
        '-X',
        'utf8',
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
