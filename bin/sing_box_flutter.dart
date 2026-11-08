import 'dart:io';
import 'dart:isolate';

/// Installs or builds the pinned native core in this plugin's package directory.
Future<void> main(List<String> arguments) async {
  if (arguments.isEmpty || arguments.first == '--help') {
    stdout.writeln('''Native core setup for sing_box_flutter.

Usage:
  dart run sing_box_flutter install --target android --release v0.2.0
  dart run sing_box_flutter install --target apple --release v0.2.0
  dart run sing_box_flutter build --target apple

install: download and verify native release artifacts.
build: compile the pinned source using the pinned Tailscale Go toolchain.
Python 3.11+ is required. Pass --help after a subcommand for its options.''');
    return;
  }
  final script = switch (arguments.first) {
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
  // Resolve caller-owned manifests before the child switches to the package.
  final forwarded = arguments.skip(1).toList();
  for (var index = 0; index < forwarded.length; index++) {
    for (final option in ['--wgc-config', '--from-dir']) {
      if (forwarded[index] == option && index + 1 < forwarded.length) {
        forwarded[index + 1] = File(forwarded[index + 1]).absolute.path;
      } else if (forwarded[index].startsWith('$option=')) {
        final path = forwarded[index].substring(option.length + 1);
        forwarded[index] = '$option=${File(path).absolute.path}';
      }
    }
  }
  try {
    final process = await Process.start(
      python,
      [executable.path, ...forwarded],
      workingDirectory: root.path,
      mode: ProcessStartMode.inheritStdio,
    );
    exitCode = await process.exitCode;
  } on ProcessException {
    stderr.writeln('Could not start the native setup tool.');
    exitCode = 69;
  }
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
