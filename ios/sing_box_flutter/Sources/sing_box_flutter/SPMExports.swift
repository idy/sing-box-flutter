// SPDX-License-Identifier: GPL-3.0-or-later
import Flutter
import Foundation
import SingBoxFlutterHost

/// The Objective-C-visible entry point used by Flutter's generated registrant.
@objc(SingBoxFlutterPlugin)
public final class SingBoxFlutterPlugin: NSObject, FlutterPlugin {
    public static func register(with registrar: FlutterPluginRegistrar) {
        SingBoxFlutterHost.SingBoxFlutterPlugin.register(with: registrar)
    }
}
