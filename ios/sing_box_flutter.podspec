Pod::Spec.new do |s|
  s.name             = 'sing_box_flutter'
  s.version          = '0.1.0'
  s.summary          = 'Native sing-box VPN integration for Flutter and iOS Packet Tunnel extensions.'
  s.description      = <<-DESC
Flutter channels manage VPN preferences and an App Group configuration handoff.
The separate PacketTunnel subspec runs the pinned libbox core in a Network Extension.
Install the native Libbox.xcframework artifact before running pod install.
                       DESC
  s.homepage         = 'https://github.com/idy/sing-box-flutter'
  s.license          = { :type => 'GPL-3.0-or-later', :file => '../LICENSE' }
  s.author           = { 'idy' => 'https://github.com/idy' }
  s.source           = { :git => 'https://github.com/idy/sing-box-flutter.git', :tag => "v#{s.version}" }
  s.platform         = :ios, '15.0'
  s.swift_version    = '5.0'
  s.module_name      = 'sing_box_flutter'
  s.static_framework = true
  s.default_subspecs = 'Plugin'
  s.preserve_paths   = 'NOTICE'
  s.pod_target_xcconfig = { 'DEFINES_MODULE' => 'YES' }

  s.subspec 'Core' do |core|
    core.source_files = 'Classes/Shared/**/*.swift'
    core.vendored_frameworks = 'Frameworks/Libbox.xcframework'
    core.frameworks = 'Network', 'NetworkExtension', 'Security', 'SystemConfiguration', 'CoreFoundation', 'UIKit'
    core.libraries = 'resolv'
    core.resource_bundles = {
      'sing_box_flutter_privacy' => ['sing_box_flutter/Sources/sing_box_flutter/PrivacyInfo.xcprivacy']
    }
  end

  # Flutter's generated plugin registrant resolves this default subspec.
  s.subspec 'Plugin' do |plugin|
    plugin.source_files = 'sing_box_flutter/Sources/sing_box_flutter/SingBoxFlutterPlugin.swift'
    plugin.dependency 'sing_box_flutter/Core'
    plugin.dependency 'Flutter'
    # CocoaPods makes separate pod variants for the app and extension. Its default
    # XCFramework copy path ignores that distinction and creates duplicate outputs.
    plugin.pod_target_xcconfig = {
      'PODS_XCFRAMEWORKS_BUILD_DIR' => '$(PODS_CONFIGURATION_BUILD_DIR)/SingBoxFlutterPluginXCFrameworks'
    }
    plugin.user_target_xcconfig = {
      'FRAMEWORK_SEARCH_PATHS' => '$(inherited) "$(PODS_CONFIGURATION_BUILD_DIR)/SingBoxFlutterPluginXCFrameworks/sing_box_flutter/Core"'
    }
  end

  # Depend on ONLY this subspec in the host's Packet Tunnel extension target.
  # It deliberately has no dependency on Flutter or an application engine.
  s.subspec 'PacketTunnel' do |tunnel|
    tunnel.source_files = 'Classes/PacketTunnel/**/*.swift'
    tunnel.dependency 'sing_box_flutter/Core'
    tunnel.pod_target_xcconfig = {
      'APPLICATION_EXTENSION_API_ONLY' => 'YES',
      'PODS_XCFRAMEWORKS_BUILD_DIR' => '$(PODS_CONFIGURATION_BUILD_DIR)/SingBoxFlutterPacketTunnelXCFrameworks'
    }
    tunnel.user_target_xcconfig = {
      'FRAMEWORK_SEARCH_PATHS' => '$(inherited) "$(PODS_CONFIGURATION_BUILD_DIR)/SingBoxFlutterPacketTunnelXCFrameworks/sing_box_flutter/Core"'
    }
  end
end
