#!/usr/bin/env ruby
# Configures the example's native targets from its host pubspec before pod install.
require 'date'
require 'yaml'
require 'xcodeproj'
require_relative '../ios/configure_spm'

module SingBoxFlutterMacOSExample
  def self.mode(directory = __dir__)
    pubspec = YAML.safe_load(File.read(File.join(directory, '..', 'pubspec.yaml')),
                            permitted_classes: [Date, Time], aliases: true)
    options = pubspec.fetch('sing_box_flutter', {})
    raise 'sing_box_flutter must be a mapping.' unless options.is_a?(Hash)
    return 'ne' unless options.key?('macos')

    macos = options['macos']
    unless macos.is_a?(Hash) && (macos.keys - ['mode']).empty? && %w[ne user_tun].include?(macos.fetch('mode', 'ne'))
      raise 'sing_box_flutter.macos requires mode: ne or user_tun.'
    end
    macos.fetch('mode', 'ne')
  end

  def self.configure(directory = __dir__)
    selected_mode = mode(directory)
    ne_mode = selected_mode == 'ne'
    project = Xcodeproj::Project.open(File.join(directory, 'Runner.xcodeproj'))
    runner = project.targets.find { |target| target.name == 'Runner' }
    raise 'Runner target is missing.' unless runner

    spm_enabled = SingBoxFlutterExampleSPM.enabled?(directory)
    # Flutter owns the aggregate package; remove it only for CocoaPods hosts.
    (spm_enabled ? [] : project.objects.to_a).each do |object|
      if object.isa == 'PBXBuildFile' && object.display_name.include?('FlutterGeneratedPluginSwiftPackage')
        object.remove_from_project
      elsif object.isa == 'XCSwiftPackageProductDependency' && object.product_name == 'FlutterGeneratedPluginSwiftPackage'
        object.remove_from_project
      elsif object.isa == 'XCLocalSwiftPackageReference' && object.relative_path.to_s.include?('FlutterGeneratedPluginSwiftPackage')
        object.remove_from_project
      elsif object.isa == 'PBXFileReference' && object.path.to_s.include?('FlutterGeneratedPluginSwiftPackage')
        object.remove_from_project
      end
    end

    extension = project.targets.find { |target| target.name == 'PacketTunnel' }
    created_extension = extension.nil?
    extension ||= project.new_target(:app_extension, 'PacketTunnel', :osx, '12.0')
    group = project.main_group.groups.find { |item| item.path == 'PacketTunnel' }
    group ||= project.main_group.new_group('PacketTunnel', 'PacketTunnel')
    ['PacketTunnelProvider.swift', 'Info.plist', 'PacketTunnel.entitlements'].each do |name|
      reference = group.files.find { |item| item.path == name } || group.new_file(name)
      if name.end_with?('.swift') && !extension.source_build_phase.files_references.include?(reference)
        extension.source_build_phase.add_file_reference(reference)
      end
    end
    runner_group = project.main_group.groups.find { |item| item.path == 'Runner' }
    ['UserTunDebugProfile.entitlements', 'UserTunRelease.entitlements'].each do |name|
      runner_group.new_file(name) unless runner_group.files.any? { |item| item.path == name }
    end

    # Flutter's template includes Pods-Runner through its project-level configs.
    # Keep those includes on Runner only, otherwise the extension sees the host's
    # Go-free sing_box_flutter module before its own PacketTunnel module.
    configs = runner_group.groups.find { |item| item.path == 'Configs' }
    warnings = configs.files.find { |item| item.path == 'Warnings.xcconfig' }
    project.build_configurations.each { |configuration| configuration.base_configuration_reference = warnings }
    runner.build_configurations.each do |configuration|
      name = configuration.name == 'Debug' ? 'Debug.xcconfig' : 'Release.xcconfig'
      configuration.base_configuration_reference = configs.files.find { |item| item.path == name }
    end
    flutter_assemble = project.targets.find { |target| target.name == 'Flutter Assemble' }
    generated = project.files.find { |item| item.path == 'ephemeral/Flutter-Generated.xcconfig' }
    flutter_assemble.build_configurations.each { |configuration| configuration.base_configuration_reference = generated }

    # Preserve signing identities and teams chosen locally by the host developer.
    [project, *project.targets].each do |target|
      target.build_configurations.each do |configuration|
        configuration.build_settings['MACOSX_DEPLOYMENT_TARGET'] = '12.0'
        # NE entitlements need a real identity selected by Xcode/provisioning.
        # Retain developer overrides, but remove Flutter's ad-hoc template default.
        if ne_mode && configuration.build_settings['CODE_SIGN_IDENTITY'] == '-'
          configuration.build_settings.delete('CODE_SIGN_IDENTITY')
        end
      end
    end
    runner.build_configurations.each do |configuration|
      debug_profile = configuration.name != 'Release'
      entitlements = debug_profile ? 'DebugProfile' : 'Release'
      entitlements = "UserTun#{entitlements}" unless ne_mode
      configuration.build_settings['CODE_SIGN_ENTITLEMENTS'] = "Runner/#{entitlements}.entitlements"
      configuration.build_settings['ENABLE_APP_SANDBOX'] = ne_mode ? 'YES' : 'NO'
    end
    extension.build_configurations.each do |configuration|
      configuration.build_settings['PRODUCT_BUNDLE_IDENTIFIER'] = 'dev.idy.singBoxFlutterExample.PacketTunnel' if created_extension
      configuration.build_settings['CODE_SIGN_STYLE'] ||= 'Automatic'
      configuration.build_settings['APPLICATION_EXTENSION_API_ONLY'] = 'YES'
      configuration.build_settings['ENABLE_APP_SANDBOX'] = 'YES'
      if created_extension
        configuration.build_settings.merge!({
          'CODE_SIGN_ENTITLEMENTS' => 'PacketTunnel/PacketTunnel.entitlements',
          'GENERATE_INFOPLIST_FILE' => 'NO',
          'INFOPLIST_FILE' => 'PacketTunnel/Info.plist',
          'PRODUCT_NAME' => '$(TARGET_NAME)',
          'PRODUCT_MODULE_NAME' => 'PacketTunnel',
          'MARKETING_VERSION' => '1.0.0',
          'CURRENT_PROJECT_VERSION' => '1',
          'SWIFT_VERSION' => '5.0',
          'CLANG_ENABLE_MODULES' => 'YES',
          'SKIP_INSTALL' => 'YES',
          'LD_RUNPATH_SEARCH_PATHS' => ['$(inherited)', '@executable_path/../Frameworks', '@executable_path/../../../../Frameworks'],
        })
      end
    end
    attributes = project.root_object.attributes['TargetAttributes'] ||= {}
    capabilities = (attributes[runner.uuid] ||= {})['SystemCapabilities'] ||= {}
    capabilities['com.apple.Sandbox'] = { 'enabled' => ne_mode ? 1 : 0 }
    capabilities['com.apple.ApplicationGroups.Mac'] = { 'enabled' => ne_mode ? 1 : 0 }
    capabilities['com.apple.NetworkExtensions'] = { 'enabled' => ne_mode ? 1 : 0 }

    embed = runner.copy_files_build_phases.find { |phase| phase.name == 'Embed App Extensions' }
    if ne_mode
      runner.add_dependency(extension) unless runner.dependencies.any? { |dependency| dependency.target == extension }
      embed ||= runner.new_copy_files_build_phase('Embed App Extensions')
      embed.dst_subfolder_spec = '13'
      unless embed.files_references.include?(extension.product_reference)
        file = embed.add_file_reference(extension.product_reference)
        file.settings = { 'ATTRIBUTES' => ['RemoveHeadersOnCopy'] }
      end
      # Copy the extension before Flutter embeds/signs its frameworks.
      runner.build_phases.delete(embed)
      flutter_embed = runner.shell_script_build_phases.find { |phase| phase.shell_script.include?('macos_assemble.sh embed') }
      index = flutter_embed && runner.build_phases.index(flutter_embed)
      runner.build_phases.insert(index || runner.build_phases.length, embed)
    else
      runner.dependencies.select { |dependency| dependency.target == extension }.each do |dependency|
        proxy = dependency.target_proxy
        dependency.remove_from_project
        proxy.remove_from_project if proxy
      end
      if embed
        embed.files.select { |file| file.file_ref == extension.product_reference }.each(&:remove_from_project)
        embed.remove_from_project if embed.files.empty?
      end
    end
    SingBoxFlutterExampleSPM.configure_extension(project, extension, enabled: spm_enabled && ne_mode)
    project.save
    puts "Configured macOS #{selected_mode}: #{ne_mode ? 'sandboxed Runner with embedded PacketTunnel' : 'unsandboxed Runner without an embedded extension'}."
    selected_mode
  end
end

SingBoxFlutterMacOSExample.configure if $PROGRAM_NAME == __FILE__
