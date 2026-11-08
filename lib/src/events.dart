/// The lifecycle of the native VPN service.
enum VpnConnectionState {
  /// No VPN connection is active.
  disconnected,

  /// The native service is establishing its tunnel.
  connecting,

  /// Both the native tunnel and sing-box core have started.
  connected,

  /// The service is releasing its tunnel and core.
  disconnecting,

  /// A native operation failed. Inspect [VpnStatus.message] for context.
  error,
}

/// An event from the native service. Events never contain configuration JSON.
sealed class SingBoxEvent {
  const SingBoxEvent();

  /// Decodes a platform event. Unknown or malformed events fail explicitly.
  factory SingBoxEvent.fromMap(Map<Object?, Object?> value) {
    switch (value['type']) {
      case 'status':
        return VpnStatus.fromMap(value);
      case 'log':
        final level = value['level'];
        final message = value['message'];
        if (level is! String || message is! String) {
          throw const FormatException('Malformed native log event.');
        }
        return SingBoxLog(level: level, message: message);
      default:
        throw const FormatException('Unknown native event type.');
    }
  }
}

/// A connection status reported by the native VPN service.
final class VpnStatus extends SingBoxEvent {
  /// Creates an immutable status value.
  const VpnStatus({required this.state, this.message});

  /// Decodes a native status response.
  factory VpnStatus.fromMap(Map<Object?, Object?> value) {
    final name = value['state'];
    final message = value['message'];
    if (name is! String || (message != null && message is! String)) {
      throw const FormatException('Malformed native VPN status.');
    }
    final states =
        VpnConnectionState.values.where((state) => state.name == name);
    if (states.isEmpty) {
      throw const FormatException('Unknown native VPN state.');
    }
    return VpnStatus(state: states.single, message: message as String?);
  }

  /// The current native connection state.
  final VpnConnectionState state;

  /// A diagnostic message, if provided by the native service.
  final String? message;

  /// Whether the native core and tunnel are connected.
  bool get isConnected => state == VpnConnectionState.connected;

  @override
  String toString() => 'VpnStatus(${state.name})';
}

/// An optional, sanitized native diagnostic log entry.
final class SingBoxLog extends SingBoxEvent {
  /// Creates a log entry.
  const SingBoxLog({required this.level, required this.message});

  /// Native severity, such as `info`, `warn`, or `error`.
  final String level;

  /// The diagnostic text. Do not treat logs as an authentication API.
  final String message;
}
