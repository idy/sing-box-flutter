import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter/sing_box_flutter.dart';

String get key => base64Encode(List.filled(32, 1));

Map<String, Object?> profileJson() => {
      'version': 1,
      'private_key': key,
      'addresses': ['10.7.0.2/32'],
      'peers': [
        {'id': 'exit', 'public_key': key, 'endpoint': '203.0.113.10:51820'},
      ],
    };

Map<String, Object?> object(Object? value) =>
    (value as Map).cast<String, Object?>();

Map<String, Object?> tun(Map<String, Object?> config) =>
    object((config['inbounds'] as List).single);

void main() {
  test('exports the canonical key validator without leaking its input', () {
    expect(() => validateWireGuardKey(key), returnsNormally);
    const secret = 'never-disclose-key-material';
    expect(
      () => validateWireGuardKey(secret),
      throwsA(isA<FormatException>()
          .having((error) => error.source, 'source', isNull)
          .having(
              (error) => error.toString(), 'message', isNot(contains(secret)))),
    );
  });

  test('conversion builds a complete single-TUN WGC endpoint configuration',
      () {
    final profile = WgcConfiguration.fromJson(profileJson());
    final config = profile.toSingBoxConfig();
    expect(config['log'], {'disabled': true});
    expect(tun(config), {
      'type': 'tun',
      'tag': 'tun-in',
      'address': ['10.7.0.2/32'],
      'mtu': 1280,
      'stack': 'gvisor',
      'auto_route': true,
      'route_address': ['0.0.0.0/0'],
      'route_exclude_address': ['203.0.113.10/32'],
      'dns_mode': 'hijack',
      'dns_address': ['198.18.0.2'],
    });
    final endpoint = object((config['endpoints'] as List).single);
    expect(endpoint, {
      'type': 'wgc',
      'tag': 'wgc',
      'configuration': profile.toJson(),
      'peer': 'exit',
      'resolver': 'dns-remote',
    });
    final route = object(config['route']);
    expect(route['auto_detect_interface'], isTrue);
    expect(route['final'], 'wgc');
    expect(route['rules'], [
      {'action': 'sniff'},
      {'port': 53, 'action': 'hijack-dns'},
      {'protocol': 'dns', 'action': 'hijack-dns'},
    ]);
    expect(jsonDecode(profile.toSingBoxConfigString()), config);
    expect(profile.toJson().containsKey('endpoints'), isFalse);
  });

  test('UDP and TCP DNS use the configured numeric upstream through WGC', () {
    final profile = WgcConfiguration.fromJson({
      ...profileJson(),
      'dns_server': '[2001:db8::53]:5353',
      'network': {
        'dns_servers': ['198.18.0.5', '2001:2::5'],
      },
    });
    final config = profile.toSingBoxConfig();
    final dns = object(config['dns']);
    expect(dns['servers'], [
      {
        'type': 'udp',
        'tag': 'dns-remote',
        'server': '2001:db8::53',
        'server_port': 5353,
        'detour': 'wgc',
      },
      {
        'type': 'tcp',
        'tag': 'dns-remote-tcp',
        'server': '2001:db8::53',
        'server_port': 5353,
        'detour': 'wgc',
      },
    ]);
    expect(dns['rules'], [
      {'network': 'tcp', 'server': 'dns-remote-tcp'},
    ]);
    expect(dns['final'], 'dns-remote');
    expect(dns['strategy'], 'ipv4_only');
    expect(tun(config)['dns_address'], ['198.18.0.5', '2001:2::5']);
  });

  test('IPv6-only and dual-stack profiles keep family defaults in sing-box',
      () {
    final ipv6 = WgcConfiguration.fromJson({
      ...profileJson(),
      'addresses': ['fd00:7::2/128'],
    }).toSingBoxConfig();
    expect(tun(ipv6)['address'], ['fd00:7::2/128']);
    expect(tun(ipv6)['route_address'], ['::/0']);
    expect(tun(ipv6)['dns_address'], ['2001:2::2']);
    expect(object(ipv6['dns'])['strategy'], 'ipv6_only');
    expect(
      object((object(ipv6['dns'])['servers'] as List).first)['server'],
      '2606:4700:4700::1111',
    );
    final dual = WgcConfiguration.fromJson({
      ...profileJson(),
      'addresses': ['fd00:7::2/128', '10.7.0.2/32'],
    }).toSingBoxConfig();
    expect(tun(dual)['route_address'], ['0.0.0.0/0', '::/0']);
    expect(tun(dual)['dns_address'], ['198.18.0.2']);
    expect(object(dual['dns'])['strategy'], 'prefer_ipv4');
  });

  test('empty includes install no routes while omitted and null use defaults',
      () {
    final empty = WgcConfiguration.fromJson({
      ...profileJson(),
      'network': {'included_routes': <String>[]},
    });
    expect(empty.network.includedRoutes, isEmpty);
    expect(tun(empty.toSingBoxConfig())['route_address'], isEmpty);
    // Upstream treats an empty route_address as default routes when auto_route
    // is enabled, so an empty list must also disable auto_route.
    expect(tun(empty.toSingBoxConfig())['auto_route'], isFalse);
    expect(
      WgcConfiguration.fromJsonString(empty.toJsonString())
          .network
          .includedRoutes,
      isEmpty,
    );
    for (final network in [
      <String, Object?>{},
      <String, Object?>{'included_routes': null},
    ]) {
      final config = WgcConfiguration.fromJson({
        ...profileJson(),
        'network': network,
      }).toSingBoxConfig();
      expect(tun(config)['auto_route'], isTrue);
      expect(tun(config)['route_address'], ['0.0.0.0/0']);
    }
  });

  test('split routes, MTU and exclusions survive selecting a different peer',
      () {
    final profile = WgcConfiguration.fromJson({
      ...profileJson(),
      'mtu': 1420,
      'peers': [
        {'id': 'exit', 'public_key': key, 'endpoint': '203.0.113.10:51820'},
        {'id': 'other', 'public_key': key, 'endpoint': '[2001:db8::1]:1234'},
        {'id': 'same-host', 'public_key': key, 'endpoint': '203.0.113.10:1234'},
      ],
      'network': {
        'included_routes': ['10.0.0.0/8', '192.168.0.0/16'],
        'excluded_routes': ['10.1.0.0/16', '203.0.113.10/32'],
      },
    });
    final config = profile.toSingBoxConfig(peer: 'other');
    expect(tun(config)['mtu'], 1420);
    expect(tun(config)['route_address'], ['10.0.0.0/8', '192.168.0.0/16']);
    expect(
      tun(config)['route_exclude_address'],
      ['10.1.0.0/16', '203.0.113.10/32', '2001:db8::1/128'],
    );
    expect(object((config['endpoints'] as List).single)['peer'], 'other');
    expect(profile.defaultPeer, 'exit');
    expect(
      jsonDecode(profile.toSingBoxConfigString(peer: 'other')),
      config,
    );
    for (final peer in ['', 'undeclared-secret-peer']) {
      expect(
        () => profile.toSingBoxConfig(peer: peer),
        throwsA(isA<FormatException>().having(
          (error) => error.toString(),
          'message',
          isNot(contains('undeclared-secret-peer')),
        )),
      );
    }
  });

  test('IPv4-mapped peer endpoints exclude their actual IPv4 host', () {
    for (final endpoint in [
      '[::ffff:203.0.113.10]:51820',
      '[::ffff:cb00:710a]:51820',
      '[0:0:0:0:0:ffff:cb00:710a]:51820',
      '[0:0:0:0:0:FFFF:CB00:710A]:51820',
    ]) {
      final profile = WgcConfiguration.fromJson({
        ...profileJson(),
        'peers': [
          {'id': 'exit', 'public_key': key, 'endpoint': endpoint},
        ],
      });
      expect(
        tun(profile.toSingBoxConfig())['route_exclude_address'],
        ['203.0.113.10/32'],
        reason: endpoint,
      );
      expect(profile.peers.single.endpoint, endpoint);
    }
  });

  test('mapped peer exclusions deduplicate IPv4 and preserve real IPv6', () {
    final profile = WgcConfiguration.fromJson({
      ...profileJson(),
      'peers': [
        {'id': 'exit', 'public_key': key, 'endpoint': '203.0.113.10:51820'},
        {
          'id': 'mapped',
          'public_key': base64Encode(List.filled(32, 2)),
          'endpoint': '[::ffff:cb00:710a]:1234',
        },
        {
          'id': 'ipv6',
          'public_key': base64Encode(List.filled(32, 3)),
          'endpoint': '[2001:db8::1]:1234',
        },
        {
          'id': 'nat64',
          'public_key': base64Encode(List.filled(32, 4)),
          'endpoint': '[64:ff9b::cb00:710a]:1234',
        },
      ],
      'network': {
        'excluded_routes': ['10.1.0.0/16', '203.0.113.10/32'],
      },
    });
    expect(
      tun(profile.toSingBoxConfig(peer: 'ipv6'))['route_exclude_address'],
      [
        '10.1.0.0/16',
        '203.0.113.10/32',
        '2001:db8::1/128',
        '64:ff9b::cb00:710a/128',
      ],
    );
  });

  test('legacy sing_box routing is preserved but cannot be silently converted',
      () {
    for (final override in [
      <String, Object?>{},
      <String, Object?>{
        'route': {'final': 'private-route'},
      },
    ]) {
      final profile = WgcConfiguration.fromJson({
        ...profileJson(),
        'sing_box': override,
      });
      expect(profile.singBox, override);
      expect(profile.toJson()['sing_box'], override);
      final failure = throwsA(isA<FormatException>()
          .having((error) => error.message, 'migration',
              contains('manual migration'))
          .having((error) => error.toString(), 'message',
              isNot(contains('private-route'))));
      expect(profile.toSingBoxConfig, failure);
      expect(profile.toSingBoxConfigString, failure);
    }
  });

  test('generated TUN options can be edited without changing the WGC profile',
      () {
    final profile = WgcConfiguration.fromJson(profileJson());
    final before = profile.toJsonString();
    final config = profile.toSingBoxConfig();
    (tun(config)['address'] as List).add('10.7.0.3/32');
    (tun(config)['route_address'] as List).clear();
    (tun(config)['dns_address'] as List).clear();
    expect(profile.toJsonString(), before);
    expect(tun(profile.toSingBoxConfig())['route_address'], ['0.0.0.0/0']);
  });

  test('custom transport options and every wrapper reach the WGC endpoint', () {
    final transport = WgcTransport(
      type: 'custom',
      options: {'private_option': 'secret'},
      base: WgcTransport(type: 'counter', base: WgcTransport()),
    );
    final json = profileJson();
    json['transport'] = transport.toJson();
    final profile = WgcConfiguration.fromJson(json);
    final endpoint =
        object((profile.toSingBoxConfig()['endpoints'] as List).single);
    expect(object(endpoint['configuration'])['transport'], transport.toJson());
    expect(profile.toString(), isNot(contains('secret')));
  });

  test('non-JSON and recursive custom options fail with redacted errors', () {
    final cycle = <String, Object?>{};
    cycle['private'] = cycle;
    for (final options in [
      <String, Object?>{'private': double.nan},
      <String, Object?>{'private': double.infinity},
      <String, Object?>{'private': Object()},
      <String, Object?>{
        'private': {1: 'secret'}
      },
      cycle,
    ]) {
      expect(
        () => WgcTransport(options: options),
        throwsA(isA<FormatException>().having(
          (error) => error.toString(),
          'message',
          isNot(contains('private')),
        )),
      );
    }
  });

  test('equivalent IPv6 tunnel addresses cannot bypass uniqueness validation',
      () {
    expect(
      () => WgcConfiguration.fromJson({
        ...profileJson(),
        'addresses': ['fd00:7::2/128', 'fd00:0007:0:0:0:0:0:2/64'],
      }),
      throwsFormatException,
    );
  });

  test('prefixes and wrong JSON types fail without leaking source', () {
    for (final address in ['10.7.0.2/032', '10.7.0.2/-1', 'fd00::2/129']) {
      expect(
        () => WgcConfiguration.fromJson({
          ...profileJson(),
          'addresses': [address],
        }),
        throwsFormatException,
      );
    }
    for (final mutation in [
      {'version': 1.0},
      {'private_key': true},
      {'mtu': 1280.0},
      {'peers': true},
      {'addresses': '10.7.0.2/32'},
      {'network': <Object?>[]},
    ]) {
      expect(
        () => WgcConfiguration.fromJson({...profileJson(), ...mutation}),
        throwsFormatException,
      );
    }
  });

  test('network route and DNS limits match the profile contract', () {
    expect(
      () => WgcNetworkSettings(
        includedRoutes: List.filled(1024, '10.0.0.0/8'),
        excludedRoutes: List.filled(1024, '2001:db8::/64'),
        dnsServers: List.filled(8, '198.18.0.2'),
      ),
      returnsNormally,
    );
    for (final network in [
      {'included_routes': List.filled(1025, '10.0.0.0/8')},
      {'excluded_routes': List.filled(1025, '2001:db8::/64')},
      {'dns_servers': List.filled(9, '198.18.0.2')},
      {'dns_servers': <String>[]},
    ]) {
      expect(
        () => WgcConfiguration.fromJson({...profileJson(), 'network': network}),
        throwsFormatException,
      );
    }
  });
}
