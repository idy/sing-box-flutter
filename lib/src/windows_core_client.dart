import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/services.dart';

import 'events.dart';

/// Private JSON-lines transport to the bundled Windows core.
///
/// Configurations travel only through the anonymous stdin pipe. Neither process
/// arguments nor diagnostics expose their contents.
class WindowsCoreClient {
  WindowsCoreClient(
    this._process, {
    required void Function(SingBoxEvent) onEvent,
    required void Function() onFailure,
    this.requestTimeout = const Duration(seconds: 60),
  })  : _onEvent = onEvent,
        _onFailure = onFailure {
    _stdout = _process.stdout
        .transform(utf8.decoder)
        .transform(const LineSplitter())
        .listen(_receive, onError: (Object _) => _fail(), onDone: _fail);
    // Upstream diagnostics can contain authentication data. Never forward them.
    _stderr = _process.stderr.listen((_) {}, onError: (Object _) => _fail());
    unawaited(_process.exitCode.then((_) => _fail(), onError: (Object _) {
      _fail();
    }));
    unawaited(_process.stdin.done.then((_) {}, onError: (Object _) {
      _fail();
    }));
  }

  /// Maximum UTF-8 frame size, including the newline consumed by Go's scanner.
  static const maxRequestBytes = 8 << 20;

  final Process _process;
  final void Function(SingBoxEvent) _onEvent;
  final void Function() _onFailure;
  final Duration requestTimeout;
  final _pending = <int, Completer<Object?>>{};
  late final StreamSubscription<String> _stdout;
  late final StreamSubscription<List<int>> _stderr;
  int _nextId = 0;
  bool _closed = false;

  Future<Object?> invoke(String method,
      [Map<String, Object?>? arguments]) async {
    if (_closed) throw _unavailable();
    final id = ++_nextId;
    final request = jsonEncode({
      'id': id,
      'method': method,
      'arguments': arguments ?? <String, Object?>{},
    });
    final frame = utf8.encode('$request\n');
    if (frame.length > maxRequestBytes) {
      throw PlatformException(
        code: 'request_too_large',
        message: 'Windows VPN requests must fit within 8 MiB.',
      );
    }
    final result = Completer<Object?>();
    _pending[id] = result;
    try {
      _process.stdin.add(frame);
    } catch (_) {
      _fail();
    }
    try {
      return await result.future.timeout(requestTimeout, onTimeout: () {
        // An ambiguous timed-out start must not leave an unobserved tunnel.
        _fail();
        throw _unavailable();
      });
    } finally {
      _pending.remove(id);
    }
  }

  void _receive(String line) {
    if (_closed) return;
    try {
      final value = jsonDecode(line);
      if (value is! Map<String, dynamic>) throw const FormatException();
      if (value.containsKey('event')) {
        final event = value['event'];
        if (event is! Map<String, dynamic>) throw const FormatException();
        _onEvent(SingBoxEvent.fromMap(event));
        return;
      }
      final id = value['id'];
      if (id is! int) throw const FormatException();
      final pending = _pending[id];
      if (pending == null || pending.isCompleted) {
        throw const FormatException();
      }
      if (value.containsKey('error')) {
        final error = value['error'];
        if (error is! Map<String, dynamic> ||
            error['code'] is! String ||
            error['message'] is! String) {
          throw const FormatException();
        }
        pending.completeError(PlatformException(
          code: error['code'] as String,
          message: error['message'] as String,
        ));
      } else if (value.containsKey('result')) {
        pending.complete(value['result']);
      } else {
        throw const FormatException();
      }
    } catch (_) {
      // JSON/parser exceptions can embed the original input. Return fixed text.
      _fail();
    }
  }

  static PlatformException _unavailable() => PlatformException(
        code: 'core_unavailable',
        message: 'The Windows VPN core exited or stopped responding. '
            'Initialize again before reconnecting.',
      );

  void _fail() {
    if (_closed) return;
    _closed = true;
    for (final pending in _pending.values) {
      if (!pending.isCompleted) pending.completeError(_unavailable());
    }
    // Closing the pipe lets the helper close sing-box and restore routes. Do not
    // force-terminate a live core: Windows cannot then run its route cleanup.
    unawaited(_process.stdin.close().catchError((Object _) {}));
    unawaited(_stdout.cancel());
    unawaited(_stderr.cancel());
    _onFailure();
  }
}
