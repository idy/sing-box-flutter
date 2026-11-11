#!/usr/bin/env ruby
# Wire only the extension; Flutter owns Runner's generated Swift package.
require 'date'
require 'yaml'
require 'xcodeproj'

module SingBoxFlutterExampleSPM
  PACKAGE = 'Flutter/ephemeral/Packages/SingBoxFlutterHost'.freeze
  PRODUCT = 'sing-box-flutter-packet-tunnel'.freeze

  def self.enabled?(directory)
    pubspec = YAML.safe_load(File.read(File.join(directory, '..', 'pubspec.yaml')),
                            permitted_classes: [Date, Time], aliases: true) || {}
    pubspec.dig('flutter', 'config', 'enable-swift-package-manager') == true
  end

  def self.configure_extension(project, extension, enabled:)
    # Older CocoaPods configuration removed the aggregate product first, leaving
    # Flutter's fixed build-file UUID orphaned. Flutter's text migration rejects
    # that UUID even though it no longer has a product or file to link. Remove
    # only this empty remnant; a valid migrated aggregate must survive intact.
    aggregate_file = project.objects_by_uuid['78A318202AECB46A00862997']
    if aggregate_file&.isa == 'PBXBuildFile' && aggregate_file.product_ref.nil? && aggregate_file.file_ref.nil?
      aggregate_file.remove_from_project
    end
    products = extension.package_product_dependencies.select { |item| item.product_name == PRODUCT }
    unless enabled
      products.each do |product|
        extension.frameworks_build_phase.files.select { |file| file.product_ref == product }.each(&:remove_from_project)
        product.remove_from_project
      end
      project.root_object.package_references.select do |reference|
        reference.isa == 'XCLocalSwiftPackageReference' && reference.relative_path == PACKAGE
      end.each(&:remove_from_project)
      return
    end

    # A previous CocoaPods install must not leak its extension module or phases
    # into the SPM extension. Runner's CocoaPods dependencies remain untouched.
    extension.build_configurations.each do |configuration|
      reference = configuration.base_configuration_reference
      configuration.base_configuration_reference = nil if reference&.path.to_s.include?('Pods-PacketTunnel')
    end
    extension.shell_script_build_phases.select { |phase| phase.name.to_s.start_with?('[CP]') }.each(&:remove_from_project)
    extension.frameworks_build_phase.files.select do |file|
      file.file_ref&.path.to_s == 'Pods_PacketTunnel.framework'
    end.each(&:remove_from_project)

    package = project.root_object.package_references.find do |reference|
      reference.isa == 'XCLocalSwiftPackageReference' && reference.relative_path == PACKAGE
    end
    unless package
      package = project.new(Xcodeproj::Project::Object::XCLocalSwiftPackageReference)
      package.relative_path = PACKAGE
      project.root_object.package_references << package
    end
    product = products.first
    unless product
      product = project.new(Xcodeproj::Project::Object::XCSwiftPackageProductDependency)
      product.product_name = PRODUCT
      extension.package_product_dependencies << product
    end
    product.package = package
    unless extension.frameworks_build_phase.files.any? { |file| file.product_ref == product }
      file = project.new(Xcodeproj::Project::Object::PBXBuildFile)
      file.product_ref = product
      extension.frameworks_build_phase.files << file
    end
  end

  def self.configure(directory = __dir__)
    project = Xcodeproj::Project.open(File.join(directory, 'Runner.xcodeproj'))
    extension = project.targets.find { |target| target.name == 'PacketTunnel' }
    raise 'PacketTunnel target is missing; configure the native example first.' unless extension
    enabled = enabled?(directory)
    configure_extension(project, extension, enabled: enabled)
    project.save
    puts "Configured iOS PacketTunnel for #{enabled ? 'SwiftPM' : 'CocoaPods'}."
  end
end

SingBoxFlutterExampleSPM.configure if $PROGRAM_NAME == __FILE__
