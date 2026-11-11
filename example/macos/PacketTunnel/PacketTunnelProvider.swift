#if canImport(SingBoxFlutterPacketTunnel)
import SingBoxFlutterPacketTunnel
#else
import sing_box_flutter
#endif

/// Runner owns Flutter; this separately signed extension owns the Go core.
final class PacketTunnelProvider: SingBoxPacketTunnelProvider {}
