import 'dart:convert';

/// Validates a canonical Base64 WireGuard key containing exactly 32 bytes.
///
/// This checks the encoding, not ownership, peer trust, or key suitability.
/// Errors never contain the supplied key.
void validateWireGuardKey(String value) {
  try {
    final decoded = base64Decode(value);
    if (decoded.length == 32 && base64Encode(decoded) == value) {
      return;
    }
  } catch (_) {
    // Return only a fixed, non-secret validation error.
  }
  throw const FormatException(
    'WireGuard keys must be canonical Base64 with 32 bytes.',
  );
}
