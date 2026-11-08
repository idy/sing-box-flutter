#!/usr/bin/env ruby
# Recreates the example's native extension target without changing global tools.
require 'xcodeproj'

project = Xcodeproj::Project.open(File.join(__dir__, 'Runner.xcodeproj'))
runner = project.targets.find { |target| target.name == 'Runner' }
raise 'Runner target is missing' unless runner

# The generated Flutter template enables SwiftPM. This example uses CocoaPods
# because its extension must select a Flutter-free PacketTunnel subspec.
project.objects.to_a.each do |object|
  if object.isa == 'PBXBuildFile' && object.display_name.include?('FlutterGeneratedPluginSwiftPackage')
    object.remove_from_project
  elsif object.isa == 'XCSwiftPackageProductDependency' && object.product_name == 'FlutterGeneratedPluginSwiftPackage'
    object.remove_from_project
  elsif object.isa == 'XCLocalSwiftPackageReference' && object.relative_path.include?('FlutterGeneratedPluginSwiftPackage')
    object.remove_from_project
  elsif object.isa == 'PBXFileReference' && object.path.to_s.include?('FlutterGeneratedPluginSwiftPackage')
    object.remove_from_project
  end
end

extension = project.targets.find { |target| target.name == 'PacketTunnel' }
extension ||= project.new_target(:app_extension, 'PacketTunnel', :ios, '15.0')
group = project.main_group.groups.find { |item| item.path == 'PacketTunnel' }
group ||= project.main_group.new_group('PacketTunnel', 'PacketTunnel')
['PacketTunnelProvider.swift', 'Info.plist', 'PacketTunnel.entitlements'].each do |name|
  reference = group.files.find { |item| item.path == name } || group.new_file(name)
  if name.end_with?('.swift') && !extension.source_build_phase.files_references.include?(reference)
    extension.source_build_phase.add_file_reference(reference)
  end
end

project.targets.each do |target|
  target.build_configurations.each do |configuration|
    configuration.build_settings.delete('DEVELOPMENT_TEAM')
    configuration.build_settings['IPHONEOS_DEPLOYMENT_TARGET'] = '15.0'
  end
end
runner.build_configurations.each do |configuration|
  configuration.build_settings['CODE_SIGN_ENTITLEMENTS'] = 'Runner/Runner.entitlements'
end
extension.build_configurations.each do |configuration|
  configuration.build_settings.merge!({
    'APPLICATION_EXTENSION_API_ONLY' => 'YES',
    'CODE_SIGN_ENTITLEMENTS' => 'PacketTunnel/PacketTunnel.entitlements',
    'GENERATE_INFOPLIST_FILE' => 'NO',
    'INFOPLIST_FILE' => 'PacketTunnel/Info.plist',
    'PRODUCT_BUNDLE_IDENTIFIER' => 'dev.idy.singBoxFlutterExample.PacketTunnel',
    'PRODUCT_NAME' => '$(TARGET_NAME)',
    'PRODUCT_MODULE_NAME' => 'PacketTunnel',
    'SWIFT_VERSION' => '5.0',
    'CLANG_ENABLE_MODULES' => 'YES',
    'SKIP_INSTALL' => 'YES',
    'TARGETED_DEVICE_FAMILY' => '1,2',
  })
end
unless runner.dependencies.any? { |dependency| dependency.target == extension }
  runner.add_dependency(extension)
end
embed = runner.copy_files_build_phases.find { |phase| phase.name == 'Embed App Extensions' }
embed ||= runner.new_copy_files_build_phase('Embed App Extensions')
embed.dst_subfolder_spec = '13'
unless embed.files_references.include?(extension.product_reference)
  file = embed.add_file_reference(extension.product_reference)
  file.settings = { 'ATTRIBUTES' => ['RemoveHeadersOnCopy'] }
end
project.save
puts 'Configured Runner and embedded PacketTunnel for CocoaPods.'
