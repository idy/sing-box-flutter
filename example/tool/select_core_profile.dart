import 'dart:convert';
import 'dart:io';

// This repository-owned example tool shares the build CLI's strict feature schema.
// ignore: implementation_imports
import 'package:sing_box_flutter/src/core_build_config.dart';
import 'package:yaml/yaml.dart';

const coreProfiles = {
  'wgc-minimal': 'TUN + direct + WGC + basic DNS',
  'wgc-tailscale': 'WGC + WireGuard + Tailscale + selectors + DoH',
  'singbox-full': 'All supported common Android/iOS capabilities',
};

/// Replaces only the top-level feature block, preserving the rest of the host.
String applyCoreProfile(String pubspec, String profile) {
  final host = _loadYaml(pubspec);
  final preset = _loadYaml(profile);
  _rejectSharedNodes(host);
  _rejectSharedNodes(preset);
  if (host is! YamlMap ||
      preset is! YamlMap ||
      preset.length != 1 ||
      !preset.containsKey('sing_box_flutter')) {
    throw const FormatException(
        'Expected a host pubspec and one feature profile');
  }
  final settings = preset['sing_box_flutter'];
  if (settings is! YamlMap ||
      settings.length != 1 ||
      !settings.containsKey('core')) {
    throw const FormatException(
        'A profile must contain only sing_box_flutter.core');
  }
  parseCoreSelection(
      settings['core']); // Reject unknown or malformed features first.
  final presetKey = preset.nodes.keys.single;
  final presetValue = preset.nodes[presetKey]!;
  var block = profile
      .substring(presetKey.span.start.offset, presetValue.span.end.offset)
      .trimRight();
  final oldSettings = host['sing_box_flutter'];
  String? macosMode;
  if (oldSettings is Map && oldSettings.containsKey('macos')) {
    macosMode = parseMacosSelection(oldSettings['macos']);
    block = '$block\n  macos:\n    mode: $macosMode';
  }
  final oldKeys =
      host.nodes.keys.where((key) => key.value == 'sing_box_flutter');
  if (oldKeys.isEmpty) {
    return _validatedCandidate('${pubspec.trimRight()}\n\n$block\n', settings);
  }
  final key = oldKeys.single;
  final value = host.nodes[key]!;
  final start = pubspec.lastIndexOf('\n', key.span.start.offset) + 1;
  final before = pubspec.substring(0, start);
  final region = pubspec.substring(start, value.span.end.offset);
  // Block-map spans include trailing comments. Keep those comments with the
  // following host setting rather than deleting them during a profile switch.
  var contentEnd = region.length;
  while (contentEnd > 0) {
    final lineStart = region.lastIndexOf('\n', contentEnd - 1) + 1;
    final line = region.substring(lineStart, contentEnd).trim();
    if (line.isNotEmpty && !line.startsWith('#')) break;
    contentEnd = lineStart > 0 ? lineStart - 1 : 0;
  }
  final after = pubspec.substring(start + contentEnd);
  // The parsed node may consume the newline before its next sibling, or end at
  // its last scalar. Normalize only that boundary, not unrelated YAML/comments.
  return _validatedCandidate(
      '$before$block\n\n${after.replaceFirst(RegExp(r'^\s*\n'), '')}', settings,
      macosMode: macosMode);
}

YamlNode _loadYaml(String text) {
  try {
    return loadYamlNode(text);
  } on YamlException catch (error) {
    throw FormatException('Invalid YAML: ${error.message}');
  }
}

void _rejectSharedNodes(YamlNode root) {
  final seen = Set<YamlNode>.identity();
  void visit(YamlNode node) {
    // Alias nodes reuse the anchor's node and source span. Splicing those spans
    // can delete a different setting or an anchor still referenced elsewhere.
    if (!seen.add(node)) {
      throw const FormatException(
          'Profile switching does not support YAML aliases or shared nodes');
    }
    if (node is YamlMap) {
      for (final entry in node.nodes.entries) {
        visit(entry.key as YamlNode);
        visit(entry.value);
      }
    } else if (node is YamlList) {
      for (final child in node.nodes) {
        visit(child);
      }
    }
  }

  visit(root);
}

String _validatedCandidate(String candidate, YamlMap settings,
    {String? macosMode}) {
  final parsed = _loadYaml(candidate);
  if (parsed is! YamlMap || parsed['sing_box_flutter'] is! YamlMap) {
    throw const FormatException(
        'Updated pubspec must contain the core profile');
  }
  final updated = parsed['sing_box_flutter'] as YamlMap;
  if (updated.length != (macosMode == null ? 1 : 2) ||
      (macosMode != null && updated['macos']?['mode'] != macosMode) ||
      !updated.containsKey('core') ||
      jsonEncode(parseCoreSelection(updated['core'])) !=
          jsonEncode(parseCoreSelection(settings['core']))) {
    throw const FormatException(
        'Updated pubspec does not match the core profile');
  }
  return candidate;
}

void main(List<String> arguments) {
  if (arguments.length != 1 || arguments.single == '--help') {
    stdout.writeln('Usage: dart run tool/select_core_profile.dart <profile>');
    for (final entry in coreProfiles.entries) {
      stdout.writeln('  ${entry.key}: ${entry.value}');
    }
    if (arguments.length == 1 && arguments.single == '--help') return;
    exitCode = 64;
    return;
  }
  final name = arguments.single;
  if (!coreProfiles.containsKey(name)) {
    stderr.writeln('Unknown core profile: $name. Use --help to list profiles.');
    exitCode = 64;
    return;
  }
  final example = File.fromUri(Platform.script).parent.parent;
  final pubspec = File('${example.path}/pubspec.yaml');
  final temporary = File('${example.path}/.pubspec.profile.$pid.tmp');
  try {
    if (FileSystemEntity.typeSync(pubspec.path, followLinks: false) !=
        FileSystemEntityType.file) {
      throw const FileSystemException('Example pubspec must be a regular file');
    }
    final profile = File('${example.path}/core-profiles/$name.yaml');
    final original = pubspec.readAsStringSync();
    final updated = applyCoreProfile(original, profile.readAsStringSync());
    if (updated != original) {
      temporary.writeAsStringSync(updated, flush: true);
      temporary.renameSync(pubspec.path);
    }
    stdout.writeln('Selected $name: ${coreProfiles[name]}');
    stdout
        .writeln('Rebuild the native core and app from the example directory:');
    stdout.writeln('  dart run sing_box_flutter build --target apple');
    stdout.writeln('  dart run sing_box_flutter build --target android');
    stdout.writeln('  flutter run');
  } on FormatException catch (error) {
    stderr.writeln('Profile not applied: ${error.message}');
    exitCode = 64;
  } on FileSystemException {
    stderr.writeln('Cannot read or update the example core profile.');
    exitCode = 66;
  } finally {
    if (temporary.existsSync()) temporary.deleteSync();
  }
}
