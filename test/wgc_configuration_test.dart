import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter/sing_box_flutter.dart';

String get key => base64Encode(List.filled(32, 1));

Map<String, Object?> configurationJson() => {
      'version': 1,
      'private_key': key,
      'addresses': ['10.7.0.2/32'],
      'peers': [
        {'id': 'exit', 'public_key': key, 'endpoint': '203.0.113.10:51820'}
      ],
    };

void main() {
  test('version 1 defaults and round trip match the core contract', () {
    final configuration = WgcConfiguration.fromJson(configurationJson());
    expect(configuration.defaultPeer, 'exit');
    expect(configuration.mtu, 1280);
    expect(configuration.transport.type, 'udp');
    expect(configuration.dnsServer, '1.1.1.1:53');
    expect(configuration.network.dnsServers, ['198.18.0.2']);
    expect(configuration.network.includedRoutes, ['0.0.0.0/0']);
    expect(
        WgcConfiguration.fromJsonString(configuration.toJsonString()).toJson(),
        configuration.toJson());
    expect(configuration.toString(), isNot(contains(key)));
  });

  test('keys, numeric endpoints, CIDRs, and peer selection are validated', () {
    for (final endpoint in [
      'vpn.invalid:51820',
      'fe80::1%en0:51820',
      '[fe80::1%en0]:51820',
      '203.0.113.1:0',
      '203.0.113.1:65536'
    ]) {
      expect(() => WgcPeer(id: 'exit', publicKey: key, endpoint: endpoint),
          throwsFormatException);
    }
    expect(
        () =>
            WgcPeer(id: 'exit', publicKey: '$key\n', endpoint: '203.0.113.1:1'),
        throwsFormatException);
    expect(
        () => WgcConfiguration.fromJson({...configurationJson(), 'version': 2}),
        throwsFormatException);
    expect(
        () => WgcConfiguration.fromJson({
              ...configurationJson(),
              'addresses': ['10.7.0.2/33']
            }),
        throwsFormatException);
    expect(
        () => WgcConfiguration.fromJson(
            {...configurationJson(), 'default_peer': 'missing'}),
        throwsFormatException);
    expect(
        () => WgcConfiguration.fromJson({...configurationJson(), 'peers': []}),
        throwsFormatException);
    expect(
        WgcPeer(id: 'v6', publicKey: key, endpoint: '[2001:db8::1]:51820').id,
        'v6');
  });

  test('defaults follow the tunnel address families', () {
    final ipv6Only = {
      ...configurationJson(),
      'addresses': ['fd00:7::2/128'],
    };
    final defaults = WgcConfiguration.fromJson(ipv6Only);
    expect(defaults.dnsServer, '[2606:4700:4700::1111]:53');
    expect(defaults.network.dnsServers, ['2001:2::2']);
    expect(defaults.network.includedRoutes, ['::/0']);

    final partial = WgcConfiguration.fromJson({
      ...ipv6Only,
      'network': {
        'excluded_routes': ['2001:db8::1/128']
      },
    });
    expect(partial.network.dnsServers, ['2001:2::2']);
    expect(partial.network.includedRoutes, ['::/0']);

    final dualStack = WgcConfiguration.fromJson({
      ...configurationJson(),
      'addresses': ['fd00:7::2/128', '10.7.0.2/32'],
    });
    expect(dualStack.dnsServer, '1.1.1.1:53');
    expect(dualStack.network.dnsServers, ['198.18.0.2']);
    expect(dualStack.network.includedRoutes, ['0.0.0.0/0', '::/0']);

    final explicit = WgcConfiguration.fromJson({
      ...ipv6Only,
      'dns_server': '[2001:db8::53]:53',
      'network': {
        'dns_servers': ['fd00:7::53']
      },
    });
    expect(explicit.dnsServer, '[2001:db8::53]:53');
    expect(explicit.network.dnsServers, ['fd00:7::53']);
    expect(explicit.toJson()['dns_server'], '[2001:db8::53]:53');
    expect(
        () => WgcConfiguration.fromJson({
              ...ipv6Only,
              'network': {'dns_servers': <String>[]},
            }),
        throwsFormatException);
  });

  test('limits match the Go schema for addresses, peers, and MTU', () {
    final addresses = List.generate(8, (index) => '10.7.0.${index + 2}/32');
    final peers = List.generate(
        32,
        (index) => {
              'id': 'peer$index',
              'public_key': base64Encode(List.filled(32, index + 1)),
              'endpoint': '203.0.113.10:51820',
            });
    for (final mtu in [1280, 1420]) {
      expect(
          WgcConfiguration.fromJson({
            ...configurationJson(),
            'addresses': addresses,
            'peers': peers,
            'mtu': mtu
          }).mtu,
          mtu);
    }
    for (final mtu in [1279, 1421]) {
      expect(
          () => WgcConfiguration.fromJson({...configurationJson(), 'mtu': mtu}),
          throwsFormatException);
    }
    expect(
        () => WgcConfiguration.fromJson({
              ...configurationJson(),
              'addresses': [...addresses, '10.7.0.10/32']
            }),
        throwsFormatException);
    expect(
        () => WgcConfiguration.fromJson({
              ...configurationJson(),
              'peers': [
                ...peers,
                {...peers.last, 'id': 'overflow'}
              ]
            }),
        throwsFormatException);
  });

  test('unknown schema fields are rejected without echoing their names', () {
    const unknown = 'private-field-must-not-be-disclosed';
    final values = <Map<String, Object?>>[
      {...configurationJson(), unknown: 'secret'},
      {
        ...configurationJson(),
        'peers': [
          {
            ...((configurationJson()['peers'] as List).first
                as Map<String, Object?>),
            unknown: 'secret'
          }
        ]
      },
      {
        ...configurationJson(),
        'transport': {'type': 'udp', unknown: 'secret'}
      },
      {
        ...configurationJson(),
        'network': {unknown: 'secret'}
      },
    ];
    for (final value in values) {
      try {
        WgcConfiguration.fromJson(value);
        fail('Unknown schema fields must fail');
      } on FormatException catch (error) {
        expect(error.toString(), isNot(contains(unknown)));
      }
    }
    final custom = WgcConfiguration.fromJson({
      ...configurationJson(),
      'transport': {
        'type': 'custom',
        'options': {'registered_option': 'value'}
      },
      'sing_box': {'inbounds': [], 'extension_owned_setting': true},
    });
    expect(custom.transport.options['registered_option'], 'value');
    expect(custom.singBox?['extension_owned_setting'], isTrue);
  });

  test('configuration owns immutable copies of custom options', () {
    final options = <String, Object?>{
      'nested': <Object?>['original']
    };
    final transport = WgcTransport(
      type: 'custom',
      options: options,
      base: WgcTransport(type: 'inner', options: options),
    );
    (options['nested'] as List)[0] = 'changed';
    for (final layer in [transport, transport.base!]) {
      expect(layer.options['nested'], ['original']);
      expect(() => layer.options['extra'] = 'value', throwsUnsupportedError);
      expect(() => (layer.options['nested'] as List).add('more'),
          throwsUnsupportedError);
    }
    final config = WgcConfiguration.fromJson(configurationJson());
    expect(() => config.addresses.add('10.7.0.3/32'), throwsUnsupportedError);
    expect(transport.toString(), isNot(contains('original')));
  });

  test('the documented counter over UDP transport round trips', () {
    final transportJson = <String, Object?>{
      'type': 'counter',
      'options': <String, Object?>{},
      'base': {
        'type': 'udp',
        'options': {'listen': ':0'},
      },
    };
    final configuration = WgcConfiguration.fromJson({
      ...configurationJson(),
      'transport': transportJson,
    });
    expect(configuration.transport.type, 'counter');
    expect(configuration.transport.base?.type, 'udp');
    expect(configuration.transport.base?.options, {'listen': ':0'});
    expect(configuration.transport.toJson(), transportJson);
    expect(
        WgcConfiguration.fromJsonString(configuration.toJsonString()).toJson(),
        configuration.toJson());
  });

  test('programmatic transport composition preserves every level', () {
    final leaf = WgcTransport(options: {'listen': ':0'});
    final transport = WgcTransport(
      type: 'custom-outer',
      options: {
        'private_option': [1, true, null]
      },
      base: WgcTransport(type: 'counter', base: leaf),
    );
    expect(transport.base?.base, same(leaf));
    expect(
        WgcTransport.fromJson(transport.toJson()).toJson(), transport.toJson());
    expect(transport.toString(), isNot(contains('private_option')));
    expect(leaf.toJson().containsKey('base'), isFalse);
    expect(
        WgcTransport.fromJson({'type': 'counter', 'base': null}).base, isNull);

    // Registration and factory/wrapper constraints belong to the native core.
    expect(
        WgcTransport.fromJson({
          'type': 'unregistered',
          'base': {'type': 'another-custom-type'},
        }).base?.type,
        'another-custom-type');
  });

  test('malformed nested transports and unknown fields fail without disclosure',
      () {
    const secret = 'private-nested-value';
    final invalidBases = <Object?>[
      secret,
      1,
      true,
      <Object?>[],
      <String, Object?>{},
      {1: 'udp'},
      {'type': 1},
      {'type': ''},
      {'type': 'udp', 'options': <Object?>[]},
      {'type': 'counter', 'base': secret},
    ];
    for (final base in invalidBases) {
      expect(
          () => WgcTransport.fromJson({'type': 'counter', 'base': base}),
          throwsA(isA<FormatException>().having((error) => error.toString(),
              'message', isNot(contains(secret)))));
    }
    const unknown = 'private-nested-field';
    for (var depth = 0; depth <= 8; depth++) {
      var json = <String, Object?>{'type': 'udp', unknown: secret};
      for (var index = 0; index < depth; index++) {
        json = {'type': 'counter', 'base': json};
      }
      expect(
          () => WgcTransport.fromJson(json),
          throwsA(isA<FormatException>()
              .having((error) => error.toString(), 'field',
                  isNot(contains(unknown)))
              .having((error) => error.toString(), 'value',
                  isNot(contains(secret)))));
    }
  });

  test('explicit transport depth accepts eight wrappers and rejects depth nine',
      () {
    var transport = WgcTransport();
    for (var index = 0; index < 8; index++) {
      transport = WgcTransport(type: 'counter', base: transport);
    }
    expect(
        WgcTransport.fromJson(transport.toJson()).toJson(), transport.toJson());
    var leaf = transport;
    for (var index = 0; index < 8; index++) {
      leaf = leaf.base!;
    }
    expect(leaf.type, 'udp');
    expect(leaf.base, isNull);
    expect(() => WgcTransport(type: 'counter', base: transport),
        throwsFormatException);
    expect(
        () => WgcTransport.fromJson(
            {'type': 'counter', 'base': transport.toJson()}),
        throwsFormatException);

    // Dart cannot infer an implicit UDP base for a custom registered name.
    var customJson = <String, Object?>{'type': 'custom-leaf'};
    for (var index = 0; index < 8; index++) {
      customJson = {'type': 'custom-layer', 'base': customJson};
    }
    expect(WgcTransport.fromJson(customJson).toJson()['type'], 'custom-layer');
    final cyclicJson = <String, Object?>{'type': 'counter'};
    cyclicJson['base'] = cyclicJson;
    expect(() => WgcTransport.fromJson(cyclicJson), throwsFormatException);
  });

  test('canonical keys keep fixed validation errors and accept zero bytes', () {
    final validKeys = [
      key,
      base64Encode(List.filled(32, 0)),
      base64Encode(List.filled(32, 255)),
    ];
    for (final value in validKeys) {
      final configuration = WgcConfiguration.fromJson({
        ...configurationJson(),
        'private_key': value,
        'peers': [
          {'id': 'exit', 'public_key': value, 'endpoint': '203.0.113.10:51820'}
        ],
      });
      expect(configuration.privateKey, value);
      expect(configuration.peers.single.publicKey, value);
    }
    final invalidKeys = [
      'private-key-never-disclosed',
      '$key\n',
      key.substring(0, key.length - 1),
      base64Encode(List.filled(31, 1)),
      base64Encode(List.filled(33, 1)),
      base64Encode(List.filled(32, 255)).replaceAll('/', '_'),
    ];
    final invalidKey = isA<FormatException>().having((error) => error.message,
        'message', 'WireGuard keys must be canonical Base64 with 32 bytes.');
    for (final value in invalidKeys) {
      expect(
          () => WgcConfiguration.fromJson(
              {...configurationJson(), 'private_key': value}),
          throwsA(invalidKey));
      expect(
          () => WgcPeer(
              id: 'exit', publicKey: value, endpoint: '203.0.113.10:51820'),
          throwsA(invalidKey));
    }
  });

  test('network settings permit outer peer exclusions from another family', () {
    final config = WgcConfiguration.fromJson({
      ...configurationJson(),
      'network': {
        'excluded_routes': ['2001:db8::1/128']
      },
    });
    expect(config.network.excludedRoutes, ['2001:db8::1/128']);
    expect(
        () => WgcConfiguration.fromJson({
              ...configurationJson(),
              'network': {
                'included_routes': ['::/0']
              },
            }),
        throwsFormatException);
  });

  test('oversized and malformed configuration errors do not echo source', () {
    const secret = 'private-value-never-returned';
    for (final source in [
      '{"private_key":"$secret",',
      jsonEncode({
        ...configurationJson(),
        'sing_box': {'secret': secret * 6000}
      })
    ]) {
      try {
        WgcConfiguration.fromJsonString(source);
        fail('Must reject invalid or oversized JSON');
      } on FormatException catch (error) {
        expect(error.toString(), isNot(contains(secret)));
      }
    }
  });
}
