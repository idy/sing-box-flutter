require_relative '../ios/core_artifacts'

artifacts = SingBoxFlutterArtifacts.new(platform: :macos, pod_root: __dir__)
ne_mode = artifacts.mode == 'ne'

Pod::Spec.new do |s|
  s.name             = 'sing_box_flutter'
  s.version          = '0.1.0'
  s.summary          = 'Native sing-box integration for Flutter macOS and Packet Tunnel extensions.'
  s.description      = <<-DESC
The host pubspec selects a Go-free Network Extension controller or an in-process
user_tun core. PacketTunnel links libbox only in the host's signed NE extension.
user_tun requires an already elevated, unsandboxed root process.
                       DESC
  s.homepage         = 'https://github.com/idy/sing-box-flutter'
  s.license          = { :type => 'GPL-3.0-or-later', :file => '../LICENSE' }
  s.author           = { 'idy' => 'https://github.com/idy' }
  s.source           = { :git => 'https://github.com/idy/sing-box-flutter.git', :tag => "v#{s.version}" }
  s.platform         = :osx, '12.0'
  s.swift_version    = '5.0'
  s.module_name      = 'sing_box_flutter'
  s.static_framework = true
  s.default_subspecs = 'Plugin'
  s.preserve_paths   = 'NOTICE'
  s.pod_target_xcconfig = { 'DEFINES_MODULE' => 'YES' }

  s.subspec 'Shared' do |shared|
    # Individual file symlinks point at the canonical iOS implementation.
    # CocoaPods does not traverse directory symlinks or ../ source globs.
    shared.source_files = 'Classes/Shared/**/*.swift'
    shared.exclude_files = 'Classes/Shared/ConfigPreflight.swift' unless ne_mode
    shared.resource_bundles = { 'sing_box_flutter_capabilities' => ['Resources/PrivacyInfo.xcprivacy'] }
    shared.script_phases = [artifacts.validation_phase, artifacts.capabilities_install_phase]
  end

  s.subspec 'Core' do |core|
    core.preserve_paths = 'NOTICE'
    core.dependency 'sing_box_flutter/Shared'
    core.frameworks = ['Network', 'Security', 'SystemConfiguration', 'CoreFoundation'] + (ne_mode ? ['NetworkExtension'] : [])
    core.libraries = 'resolv'
    core.script_phase = {
      :name => 'Check selected core SDK slice',
      :execution_position => :before_compile,
      :input_files => artifacts.recipe_input_files,
      :script => <<-SCRIPT
set -eu
case "${PLATFORM_NAME}" in
  macosx) ;;
  *) printf 'The selected Go core requires the macOS SDK. Rebuild with --target macos and run pod install.\\n' >&2; exit 1 ;;
esac
      SCRIPT
    }
    search_path = artifacts.slice_paths.fetch('macosx').gsub('\\', '\\\\').gsub('"', '\\"')
    core.pod_target_xcconfig = {
      'OTHER_SWIFT_FLAGS' => '$(inherited) -D SINGBOX_SELECTABLE_CORE',
      'FRAMEWORK_SEARCH_PATHS[sdk=macosx*]' => "$(inherited) \"#{search_path}\""
    }
    core.user_target_xcconfig = {
      'FRAMEWORK_SEARCH_PATHS[sdk=macosx*]' => "$(inherited) \"#{search_path}\"",
      'OTHER_LDFLAGS' => '$(inherited) -framework Libbox'
    }
  end

  s.subspec 'Plugin' do |plugin|
    plugin.source_files = ne_mode ? 'Classes/NE/SingBoxFlutterPlugin.swift' : ['Classes/UserTun/**/*.swift', 'Classes/NativeCore.swift']
    plugin.dependency 'sing_box_flutter/Shared'
    plugin.dependency 'FlutterMacOS'
    if ne_mode
      plugin.frameworks = 'NetworkExtension'
    else
      plugin.dependency 'sing_box_flutter/Core'
    end
  end

  if ne_mode
    # Only this subspec goes in the separately signed host extension target.
    s.subspec 'PacketTunnel' do |tunnel|
      tunnel.source_files = 'Classes/PacketTunnel/**/*.swift'
      tunnel.dependency 'sing_box_flutter/Shared'
      tunnel.dependency 'sing_box_flutter/Core'
      tunnel.pod_target_xcconfig = { 'APPLICATION_EXTENSION_API_ONLY' => 'YES' }
    end
  end
end
