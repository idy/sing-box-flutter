import 'dart:convert';
import 'dart:io';

import 'wireguard_key.dart';

/// Maximum size of the serialized legacy WGC version 1 profile, in UTF-8 bytes.
const int wgcMaximumConfigurationBytes = 128 * 1024;

/// A WGC version 1 profile and its migration to complete sing-box JSON.
///
/// [toJson] preserves the legacy profile schema. Use [toSingBoxConfig] or
/// [toSingBoxConfigString] when starting sing-box. Keys and transport options
/// are never included in [toString], but both JSON representations contain them.
final class WgcConfiguration {
  WgcConfiguration({
    required this.privateKey,
    required List<String> addresses,
    required List<WgcPeer> peers,
    String? defaultPeer,
    WgcTransport? transport,
    this.mtu = 1280,
    String? dnsServer,
    WgcNetworkSettings? network,
    Map<String, Object?>? singBox,
  })  : addresses = List.unmodifiable(addresses),
        peers = List.unmodifiable(peers),
        defaultPeer = defaultPeer ?? (peers.isEmpty ? '' : peers.first.id),
        transport = transport ?? WgcTransport(),
        dnsServer = dnsServer ?? _defaultUpstreamDns(addresses),
        network = (network ?? WgcNetworkSettings())._resolveDefaults(addresses),
        singBox = singBox == null ? null : _freezeObject(singBox) {
    validateWireGuardKey(privateKey);
    if (addresses.isEmpty ||
        addresses.length > 8 ||
        peers.isEmpty ||
        peers.length > 32) {
      throw const FormatException(
        'Configuration needs addresses and peers within supported limits.',
      );
    }
    final families = addresses.map(_cidr).toSet();
    if (addresses
            .map((value) => _ip(value.split('/').first).rawAddress.join(','))
            .toSet()
            .length !=
        addresses.length) {
      throw const FormatException('Tunnel addresses must be unique.');
    }
    final ids = peers.map((peer) => peer.id).toSet();
    if (ids.length != peers.length || !ids.contains(this.defaultPeer)) {
      throw const FormatException(
        'Peer identifiers must be unique and include the default peer.',
      );
    }
    if (mtu < 1280 || mtu > 1420) {
      throw const FormatException(
        'The tunnel MTU is outside the supported range.',
      );
    }
    _endpoint(this.dnsServer);
    for (final route in this.network.includedRoutes!) {
      if (!families.contains(_cidr(route))) {
        throw const FormatException(
          'An included route needs a matching tunnel address family.',
        );
      }
    }
    if (utf8.encode(jsonEncode(toJson())).length >
        wgcMaximumConfigurationBytes) {
      throw const FormatException('Configuration exceeds 128 KiB.');
    }
  }

  factory WgcConfiguration.fromJsonString(String source) {
    if (utf8.encode(source).length > wgcMaximumConfigurationBytes) {
      throw const FormatException('Configuration exceeds 128 KiB.');
    }
    try {
      return WgcConfiguration.fromJson(_object(jsonDecode(source)));
    } catch (_) {
      throw const FormatException('Invalid WGC version 1 configuration.');
    }
  }

  factory WgcConfiguration.fromJson(Map<String, Object?> json) {
    _rejectUnknownFields(json, _configurationFields);
    if (json['version'] != 1 || json['version'] is! int) {
      throw const FormatException('Only configuration version 1 is supported.');
    }
    final peerValues = json['peers'];
    if (peerValues is! List) {
      throw const FormatException('Configuration peers must be a list.');
    }
    return WgcConfiguration(
      privateKey: _string(json['private_key']),
      addresses: _strings(json['addresses']),
      peers:
          peerValues.map((value) => WgcPeer.fromJson(_object(value))).toList(),
      defaultPeer: _optionalString(json['default_peer']),
      transport: json['transport'] == null
          ? null
          : WgcTransport.fromJson(_object(json['transport'])),
      mtu: json['mtu'] == null ? 1280 : _integer(json['mtu']),
      dnsServer: _optionalString(json['dns_server']),
      network: json['network'] == null
          ? null
          : WgcNetworkSettings.fromJson(_object(json['network'])),
      singBox: json['sing_box'] == null ? null : _object(json['sing_box']),
    );
  }

  final String privateKey;
  final List<String> addresses;
  final List<WgcPeer> peers;
  final String defaultPeer;
  final WgcTransport transport;
  final int mtu;

  /// Numeric upstream DNS endpoint, resolved to an address-family default.
  final String dnsServer;
  final WgcNetworkSettings network;

  /// Preserved legacy override, for inspection and explicit migration only.
  ///
  /// Conversion to complete sing-box JSON rejects every non-null override,
  /// including an empty object. Move its routing, DNS, and inbound policy into
  /// the outer sing-box JSON before removing it from the WGC profile.
  final Map<String, Object?>? singBox;

  int get version => 1;

  /// Serializes the legacy version 1 profile, including any [singBox] override.
  Map<String, Object?> toJson() => {
        'version': version,
        'private_key': privateKey,
        'addresses': addresses,
        'peers': peers.map((peer) => peer.toJson()).toList(),
        'default_peer': defaultPeer,
        'transport': transport.toJson(),
        'mtu': mtu,
        'dns_server': dnsServer,
        'network': network.toJson(),
        if (singBox != null) 'sing_box': singBox,
      };

  String toJsonString() => jsonEncode(toJson());

  /// Generates a complete sing-box 1.14.4 configuration with one WGC endpoint.
  ///
  /// [peer] must name a declared peer; omission selects [defaultPeer]. The TUN
  /// uses the gVisor stack, the profile's addresses and MTU, and its included and
  /// excluded routes. Every declared outer peer also receives a host exclusion.
  /// An explicit empty included-routes list disables automatic route capture.
  /// UDP and TCP DNS use [dnsServer] through WGC and port 53 is hijacked.
  ///
  /// Legacy [singBox] overrides require manual migration and cause a
  /// [FormatException]. Transport registration is checked by the native core.
  /// The returned map may be composed with other sing-box endpoints and rules;
  /// submit the resulting JSON to native configuration validation before use.
  Map<String, Object?> toSingBoxConfig({String? peer}) {
    if (singBox != null) {
      throw const FormatException(
        'Legacy sing_box overrides require manual migration to a complete '
        'sing-box configuration. See doc/wgc.md.',
      );
    }
    final selectedPeer = peer ?? defaultPeer;
    if (!peers.any((candidate) => candidate.id == selectedPeer)) {
      throw const FormatException('The selected WGC peer must be declared.');
    }
    final upstream = _endpoint(dnsServer);
    final includes = network.includedRoutes!;
    final excludes = <String>{
      ...network.excludedRoutes,
      for (final peer in peers) _hostRoute(_endpoint(peer.endpoint).address),
    };
    final families = addresses.map(_cidr).toSet();
    return {
      'log': {'disabled': true},
      'dns': {
        'servers': [
          for (final protocol in ['udp', 'tcp'])
            {
              'type': protocol,
              'tag': protocol == 'udp' ? 'dns-remote' : 'dns-remote-tcp',
              'server': upstream.address.address,
              'server_port': upstream.port,
              'detour': 'wgc',
            },
        ],
        'rules': [
          {'network': 'tcp', 'server': 'dns-remote-tcp'},
        ],
        'final': 'dns-remote',
        'strategy': families.length == 2
            ? 'prefer_ipv4'
            : families.contains(InternetAddressType.IPv4)
                ? 'ipv4_only'
                : 'ipv6_only',
      },
      'inbounds': [
        {
          'type': 'tun',
          'tag': 'tun-in',
          'address': addresses.toList(),
          'mtu': mtu,
          'stack': 'gvisor',
          'auto_route': includes.isNotEmpty,
          'route_address': includes.toList(),
          'route_exclude_address': excludes.toList(),
          'dns_mode': 'hijack',
          'dns_address': network.dnsServers!.toList(),
        },
      ],
      'endpoints': [
        {
          'type': 'wgc',
          'tag': 'wgc',
          'configuration': toJson(),
          'peer': selectedPeer,
          'resolver': 'dns-remote',
        },
      ],
      'route': {
        'auto_detect_interface': true,
        'rules': [
          {'action': 'sniff'},
          {'port': 53, 'action': 'hijack-dns'},
          {'protocol': 'dns', 'action': 'hijack-dns'},
        ],
        'final': 'wgc',
      },
    };
  }

  /// Encodes [toSingBoxConfig], including private keys and transport options.
  String toSingBoxConfigString({String? peer}) =>
      jsonEncode(toSingBoxConfig(peer: peer));

  @override
  String toString() => 'WgcConfiguration(version: 1, peers: ${peers.length})';
}

final class WgcPeer {
  WgcPeer({required this.id, required this.publicKey, required this.endpoint}) {
    if (id.isEmpty ||
        utf8.encode(id).length > 128 ||
        id.runes.any((value) => value < 32)) {
      throw const FormatException('Peer identifier is invalid.');
    }
    validateWireGuardKey(publicKey);
    _endpoint(endpoint);
  }

  factory WgcPeer.fromJson(Map<String, Object?> json) {
    _rejectUnknownFields(json, const {'id', 'public_key', 'endpoint'});
    return WgcPeer(
      id: _string(json['id']),
      publicKey: _string(json['public_key']),
      endpoint: _string(json['endpoint']),
    );
  }

  final String id;
  final String publicKey;

  /// Numeric, unzoned host and port. IPv6 hosts require brackets.
  final String endpoint;

  Map<String, Object?> toJson() =>
      {'id': id, 'public_key': publicKey, 'endpoint': endpoint};

  @override
  String toString() => 'WgcPeer()';
}

/// Native transport selection; registration and option schemas belong to Go.
///
/// Unknown names are preserved for native validation, never replaced with UDP.
/// A wrapper can have at most eight explicit [base] layers.
final class WgcTransport {
  WgcTransport({
    this.type = 'udp',
    Map<String, Object?> options = const {},
    this.base,
  }) : options = _freezeObject(options) {
    if (type.isEmpty ||
        utf8.encode(type).length > 128 ||
        type.runes.any((value) => value <= 32)) {
      throw const FormatException('Transport name is invalid.');
    }
    _validateDepth(0);
  }

  factory WgcTransport.fromJson(Map<String, Object?> json) =>
      WgcTransport._fromJson(json, 0);

  factory WgcTransport._fromJson(Map<String, Object?> json, int depth) {
    _checkDepth(depth);
    _rejectUnknownFields(json, const {'type', 'options', 'base'});
    return WgcTransport(
      type: _string(json['type']),
      options: json['options'] == null ? const {} : _object(json['options']),
      base: json['base'] == null
          ? null
          : WgcTransport._fromJson(_object(json['base']), depth + 1),
    );
  }

  final String type;
  final Map<String, Object?> options;
  final WgcTransport? base;

  Map<String, Object?> toJson() => {
        'type': type,
        'options': options,
        if (base != null) 'base': base!.toJson(),
      };

  void _validateDepth(int depth) {
    _checkDepth(depth);
    base?._validateDepth(depth + 1);
  }

  static void _checkDepth(int depth) {
    if (depth > 8) {
      throw const FormatException('Transport nesting exceeds eight wrappers.');
    }
  }

  @override
  String toString() => 'WgcTransport()';
}

final class WgcNetworkSettings {
  WgcNetworkSettings({
    List<String>? dnsServers,
    List<String>? includedRoutes,
    List<String> excludedRoutes = const [],
  })  : dnsServers = dnsServers == null ? null : List.unmodifiable(dnsServers),
        includedRoutes =
            includedRoutes == null ? null : List.unmodifiable(includedRoutes),
        excludedRoutes = List.unmodifiable(excludedRoutes) {
    if ((dnsServers != null && (dnsServers.isEmpty || dnsServers.length > 8)) ||
        (includedRoutes?.length ?? 0) > 1024 ||
        excludedRoutes.length > 1024) {
      throw const FormatException('Network settings exceed supported limits.');
    }
    for (final server in [...?dnsServers]) {
      _ip(server);
    }
    for (final route in [...?includedRoutes, ...excludedRoutes]) {
      _cidr(route);
    }
  }

  factory WgcNetworkSettings.fromJson(Map<String, Object?> json) {
    _rejectUnknownFields(
      json,
      const {'dns_servers', 'included_routes', 'excluded_routes'},
    );
    return WgcNetworkSettings(
      dnsServers:
          json['dns_servers'] == null ? null : _strings(json['dns_servers']),
      includedRoutes: json['included_routes'] == null
          ? null
          : _strings(json['included_routes']),
      excludedRoutes: json['excluded_routes'] == null
          ? const []
          : _strings(json['excluded_routes']),
    );
  }

  /// DNS addresses presented by the TUN, not upstream resolver endpoints.
  ///
  /// Null selects the tunnel's address-family default. [WgcConfiguration.network]
  /// always holds a resolved list. [WgcConfiguration.dnsServer] is the upstream.
  final List<String>? dnsServers;

  /// Null selects default routes for every tunnel address family.
  ///
  /// An empty list installs no included routes. [WgcConfiguration.network]
  /// always holds a resolved list.
  final List<String>? includedRoutes;

  /// Routes bypassing the VPN; every outer peer host is also excluded on export.
  final List<String> excludedRoutes;

  WgcNetworkSettings _resolveDefaults(List<String> addresses) {
    final families = addresses.map(_cidr).toSet();
    final hasIPv4 = families.contains(InternetAddressType.IPv4);
    return WgcNetworkSettings(
      dnsServers: dnsServers ?? [hasIPv4 ? '198.18.0.2' : '2001:2::2'],
      includedRoutes: includedRoutes ??
          [
            if (hasIPv4) '0.0.0.0/0',
            if (families.contains(InternetAddressType.IPv6)) '::/0',
          ],
      excludedRoutes: excludedRoutes,
    );
  }

  Map<String, Object?> toJson() => {
        if (dnsServers != null) 'dns_servers': dnsServers,
        if (includedRoutes != null) 'included_routes': includedRoutes,
        'excluded_routes': excludedRoutes,
      };
}

String _defaultUpstreamDns(List<String> addresses) =>
    addresses.map(_cidr).contains(InternetAddressType.IPv4)
        ? '1.1.1.1:53'
        : '[2606:4700:4700::1111]:53';

String _hostRoute(InternetAddress address) =>
    '${address.address}/${address.type == InternetAddressType.IPv4 ? 32 : 128}';

const _configurationFields = {
  'version',
  'private_key',
  'addresses',
  'peers',
  'default_peer',
  'transport',
  'mtu',
  'dns_server',
  'network',
  'sing_box',
};

// Caller-provided field names and values never appear in validation errors.
void _rejectUnknownFields(Map<String, Object?> json, Set<String> allowed) {
  if (!allowed.containsAll(json.keys)) {
    throw const FormatException('Configuration contains an unsupported field.');
  }
}

InternetAddress _ip(String value) {
  if (value.isEmpty || value.contains('%') || value.trim() != value) {
    throw const FormatException(
        'Network addresses must be numeric and unzoned.');
  }
  final address = InternetAddress.tryParse(value);
  if (address == null) {
    throw const FormatException(
        'Network addresses must be numeric and unzoned.');
  }
  return address;
}

InternetAddressType _cidr(String value) {
  final parts = value.split('/');
  if (parts.length != 2 || !RegExp(r'^(0|[1-9]\d*)$').hasMatch(parts[1])) {
    throw const FormatException(
      'Tunnel addresses and routes require CIDR notation.',
    );
  }
  final type = _ip(parts[0]).type;
  final prefix = int.tryParse(parts[1]);
  if (prefix == null ||
      prefix < 0 ||
      prefix > (type == InternetAddressType.IPv4 ? 32 : 128)) {
    throw const FormatException(
      'CIDR prefix is outside the address family range.',
    );
  }
  return type;
}

({InternetAddress address, int port}) _endpoint(String value) {
  String host;
  String port;
  if (value.startsWith('[')) {
    final end = value.indexOf(']:');
    if (end <= 1) {
      throw const FormatException('Endpoint needs a numeric host and port.');
    }
    host = value.substring(1, end);
    port = value.substring(end + 2);
    if (_ip(host).type != InternetAddressType.IPv6) {
      throw const FormatException(
        'Bracketed endpoint must contain an IPv6 address.',
      );
    }
  } else {
    final parts = value.split(':');
    if (parts.length != 2) {
      throw const FormatException('Endpoint needs a numeric host and port.');
    }
    host = parts[0];
    port = parts[1];
    if (_ip(host).type != InternetAddressType.IPv4) {
      throw const FormatException('IPv6 endpoints require brackets.');
    }
  }
  final number = int.tryParse(port);
  if (!RegExp(r'^\d+$').hasMatch(port) ||
      number == null ||
      number < 1 ||
      number > 65535) {
    throw const FormatException('Endpoint port must be between 1 and 65535.');
  }
  return (address: _ip(host), port: number);
}

Map<String, Object?> _object(Object? value) {
  if (value is! Map || value.keys.any((key) => key is! String)) {
    throw const FormatException('Configuration requires a JSON object.');
  }
  return Map<String, Object?>.from(value);
}

String _string(Object? value) {
  if (value is! String) {
    throw const FormatException('Configuration string is missing or invalid.');
  }
  return value;
}

String? _optionalString(Object? value) => value == null ? null : _string(value);

List<String> _strings(Object? value) {
  if (value is! List) {
    throw const FormatException('Configuration requires a string list.');
  }
  return value.map(_string).toList();
}

int _integer(Object? value) {
  if (value is! int) {
    throw const FormatException('Configuration requires an integer.');
  }
  return value;
}

Map<String, Object?> _freezeObject(Map<String, Object?> value) =>
    _freezeJson(value, 0) as Map<String, Object?>;

Object? _freezeJson(Object? value, int depth) {
  if (depth > 32) {
    throw const FormatException('Configuration JSON is too deeply nested.');
  }
  if (value == null || value is String || value is bool || value is int) {
    return value;
  }
  if (value is double && value.isFinite) {
    return value;
  }
  if (value is List) {
    return List<Object?>.unmodifiable(
      value.map((item) => _freezeJson(item, depth + 1)),
    );
  }
  if (value is Map && value.keys.every((key) => key is String)) {
    return Map<String, Object?>.unmodifiable(value.map(
      (key, item) => MapEntry(key as String, _freezeJson(item, depth + 1)),
    ));
  }
  throw const FormatException(
      'Configuration options must contain JSON values.');
}
