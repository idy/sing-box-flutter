// SPDX-License-Identifier: GPL-3.0-or-later
import FlutterMacOS
import Foundation
import SingBoxFlutterHost

/// Registers the NE or user-TUN controller selected for this host.
public final class SingBoxFlutterPlugin: NSObject, FlutterPlugin {
    public static func register(with registrar: FlutterPluginRegistrar) {
        SingBoxFlutterHost.SingBoxFlutterPlugin.register(with: registrar)
    }
}
