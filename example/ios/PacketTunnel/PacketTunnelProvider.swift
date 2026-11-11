#if canImport(SingBoxFlutterPacketTunnel)
import SingBoxFlutterPacketTunnel
#else
import sing_box_flutter
#endif

/// The host application's extension owns this entry point; Flutter stays in Runner.
final class PacketTunnelProvider: SingBoxPacketTunnelProvider {}
