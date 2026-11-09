import 'dart:convert';
import 'dart:io';

import 'package:crypto/crypto.dart';
import 'package:path/path.dart' as p;
import 'package:yaml/yaml.dart';

/// Supported component names at the pinned upstream revision.
const coreFeatureNames = <String, String>{
  'inbounds':
      'tun direct socks http mixed shadowsocks snell vmess vless trojan naive shadowtls anytls hysteria hysteria2 tuic cloudflared redirect tproxy',
  'outbounds':
      'direct bridge block selector urltest socks http shadowsocks snell vmess vless trojan naive tor ssh shadowtls anytls hysteria hysteria2 tuic',
  'endpoints':
      'wireguard tailscale openconnect openvpn-client openvpn-server wgc',
  'dns':
      'udp tcp tls https quic h3 local hosts fakeip mdns dhcp resolved tailscale openconnect openvpn',
  'transports': 'http ws quic grpc httpupgrade',
  'tls': 'utls reality ech',
  'api': 'clash_api v2ray_api',
  'services':
      'api resolved ssm-api hysteria-realm derp ccm ocm oom-killer usbip-server usbip-client',
  'certificate_providers': 'acme tailscale cloudflare-origin-ca',
};

/// Canonical requested features. Empty lists are never replaced by defaults.
Map<String, List<String>> parseCoreSelection(Object? value) {
  if (value is! Map) {
    throw const FormatException('sing_box_flutter.core must be a mapping');
  }
  for (final key in value.keys) {
    if (!coreFeatureNames.containsKey(key)) {
      throw FormatException('Unknown core category: $key');
    }
  }
  final result = <String, List<String>>{};
  final keys = coreFeatureNames.keys.toList()..sort();
  for (final key in keys) {
    const defaults = {
      'inbounds': ['tun'],
      'outbounds': ['direct'],
      'dns': ['local', 'tcp', 'udp'],
    };
    final entries = value.containsKey(key) ? value[key] : defaults[key] ?? [];
    if (entries is! List || entries.any((entry) => entry is! String)) {
      throw FormatException('core.$key must be a list of feature names');
    }
    final names = entries.cast<String>().toList();
    if (names.toSet().length != names.length) {
      throw FormatException('Duplicate feature in core.$key');
    }
    for (final name in names) {
      if (!coreFeatureNames[key]!.split(' ').contains(name)) {
        throw FormatException('Unknown core.$key feature: $name');
      }
    }
    result[key] = names..sort();
  }
  return result;
}

/// Finds the nearest host manifest, including when called in a child directory.
File? findHostPubspec(Directory directory) {
  var current = directory.absolute;
  while (true) {
    final candidate = File(p.join(current.path, 'pubspec.yaml'));
    if (candidate.existsSync()) return candidate;
    final parent = current.parent;
    if (parent.path == current.path) return null;
    current = parent;
  }
}

/// Features and source identity captured from one immutable pubspec read.
final class CoreBuildSnapshot {
  CoreBuildSnapshot._(
      this.pubspec, this.pubspecSha256, Map<String, List<String>> features)
      : features = Map.unmodifiable(features.map(
            (key, values) => MapEntry(key, List<String>.unmodifiable(values))));

  final File pubspec;
  final String pubspecSha256;
  final Map<String, List<String>> features;
}

/// Parse and hash the exact same bytes, before any subprocess can run.
/// A missing core block preserves legacy full install/build behavior.
CoreBuildSnapshot? readCoreBuildSnapshot(File pubspec) {
  final canonical = File(pubspec.resolveSymbolicLinksSync());
  final bytes = canonical.readAsBytesSync();
  final selection = _parseCorePubspec(utf8.decode(bytes));
  if (selection == null) return null;
  return CoreBuildSnapshot._(
      canonical, sha256.convert(bytes).toString(), selection);
}

Map<String, List<String>>? _parseCorePubspec(String source) {
  final Object? document;
  try {
    document = loadYaml(source);
  } on YamlException catch (error) {
    throw FormatException('Invalid pubspec YAML: ${error.message}');
  }
  if (document is! Map) {
    throw const FormatException('Host pubspec must be a mapping');
  }
  if (!document.containsKey('sing_box_flutter')) return null;
  final plugin = document['sing_box_flutter'];
  if (plugin is! Map) {
    throw const FormatException('sing_box_flutter must be a mapping');
  }
  for (final key in plugin.keys) {
    if (key != 'core') {
      throw FormatException('Unknown sing_box_flutter key: $key');
    }
  }
  if (!plugin.containsKey('core')) return null;
  return parseCoreSelection(plugin['core']);
}

/// Write a private, complete build request that another invocation cannot replace.
/// The caller owns this file's temporary directory and must remove it on exit.
File writeCoreBuildRequest(CoreBuildSnapshot snapshot) {
  final directory =
      _coreCacheDirectory(snapshot.pubspec.parent).createTempSync('request-');
  try {
    return File(p.join(directory.path, 'request.json'))
      ..writeAsStringSync(
          '${jsonEncode({
                'schema': 1,
                'pubspec': {
                  'path': snapshot.pubspec.path,
                  'sha256': snapshot.pubspecSha256,
                },
                'features': snapshot.features,
              })}\n',
          flush: true);
  } catch (_) {
    directory.deleteSync(recursive: true);
    rethrow;
  }
}

Directory _coreCacheDirectory(Directory project) {
  final directory = Directory(
      p.join(project.absolute.path, '.dart_tool', 'sing_box_flutter'));
  for (var current = directory;
      current.path != project.absolute.path;
      current = current.parent) {
    if (FileSystemEntity.typeSync(current.path, followLinks: false) ==
        FileSystemEntityType.link) {
      throw FileSystemException(
          'The host core cache must not traverse symlinks', current.path);
    }
  }
  directory.createSync(recursive: true);
  return directory.absolute;
}
