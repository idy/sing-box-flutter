require_relative 'core_artifacts'

artifacts = SingBoxFlutterArtifacts.new(platform: :ios, pod_root: __dir__)
selectable = artifacts.selectable
framework_path = artifacts.framework_path
capabilities_path = artifacts.capabilities_path
recipe_input_files = artifacts.recipe_input_files
slice_paths = artifacts.slice_paths

Pod::Spec.new do |s|
  s.name             = 'sing_box_flutter'
  s.version          = '0.1.0'
  s.summary          = 'Native sing-box VPN integration for Flutter and iOS Packet Tunnel extensions.'
  s.description      = <<-DESC
Flutter channels manage VPN preferences and an App Group configuration handoff.
Only the PacketTunnel subspec links the selected libbox core in a Network Extension.
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

  s.subspec 'Shared' do |shared|
    shared.source_files = 'Classes/Shared/**/*.swift'
    privacy = 'sing_box_flutter/Sources/sing_box_flutter/PrivacyInfo.xcprivacy'
    if selectable
      # The bundle target creates the directory first; this pod target depends on it.
      # Do not list the fallback JSON here: two producers of the same output race.
      shared.resource_bundles = { 'sing_box_flutter_capabilities' => [privacy] }
      shared.script_phases = [artifacts.validation_phase, artifacts.capabilities_install_phase]
    else
      shared.resource_bundles = {
        'sing_box_flutter_capabilities' => [capabilities_path],
        'sing_box_flutter_privacy' => [privacy]
      }
    end
  end

  s.subspec 'Core' do |core|
    # Selectable static slices are linked through SDK-specific search paths;
    # preserve their attribution and keep Core a valid non-empty pod subspec.
    core.preserve_paths = 'NOTICE'
    core.vendored_frameworks = framework_path unless selectable
    core.frameworks = 'Network', 'NetworkExtension', 'Security', 'SystemConfiguration', 'CoreFoundation', 'UIKit'
    core.libraries = 'resolv'
    if selectable
      core.dependency 'sing_box_flutter/Shared'
      core.script_phase = {
        :name => 'Check selected core SDK slice',
        :execution_position => :before_compile,
        :input_files => recipe_input_files,
        :script => <<-SCRIPT
set -eu
case "${PLATFORM_NAME}" in
  #{slice_paths.keys.join('|')}) ;;
  *) printf 'The selected Go core has no slice for this SDK. Rebuild with --apple-platform ios,iossimulator and run pod install.\\n' >&2; exit 1 ;;
esac
        SCRIPT
      }
      search_paths = slice_paths.transform_values { |path| path.gsub('\\', '\\\\').gsub('"', '\\"') }
      core.pod_target_xcconfig = {
        'OTHER_SWIFT_FLAGS' => '$(inherited) -D SINGBOX_SELECTABLE_CORE',
        'FRAMEWORK_SEARCH_PATHS[sdk=iphoneos*]' => "$(inherited) \"#{search_paths['iphoneos']}\"",
        'FRAMEWORK_SEARCH_PATHS[sdk=iphonesimulator*]' => "$(inherited) \"#{search_paths['iphonesimulator']}\""
      }
      core.user_target_xcconfig = {
        'FRAMEWORK_SEARCH_PATHS[sdk=iphoneos*]' => "$(inherited) \"#{search_paths['iphoneos']}\"",
        'FRAMEWORK_SEARCH_PATHS[sdk=iphonesimulator*]' => "$(inherited) \"#{search_paths['iphonesimulator']}\"",
        'OTHER_LDFLAGS' => '$(inherited) -framework Libbox'
      }
    end
  end

  # Flutter's generated plugin registrant resolves this default subspec.
  s.subspec 'Plugin' do |plugin|
    plugin.source_files = 'sing_box_flutter/Sources/sing_box_flutter/SingBoxFlutterPlugin.swift'
    plugin.dependency 'sing_box_flutter/Shared'
    plugin.dependency 'Flutter'
    plugin.frameworks = 'NetworkExtension'
  end

  # Depend on ONLY this subspec in the host's Packet Tunnel extension target.
  s.subspec 'PacketTunnel' do |tunnel|
    tunnel.source_files = 'Classes/PacketTunnel/**/*.swift'
    tunnel.dependency 'sing_box_flutter/Shared'
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
