# SPDX-License-Identifier: GPL-3.0-or-later
require 'cocoapods'

# Exercise actual CocoaPods target merging and Xcode phase integration for each
# Apple platform. The callers retain their independent expected output layouts.
module SingBoxFlutterPodspecContract
  def self.phase_scripts(spec, names, host:, manifest:, platform_name:, deployment_target:, expected_output:)
    specs = names.map { |name| spec.subspec_by_name("#{spec.name}/#{name}") }
    podfile = Pod::Podfile.new { target('Host') { platform platform_name, deployment_target } }
    pod_target = Pod::PodTarget.new(Pod::Sandbox.new(host.join('phase sandbox')), Pod::BuildType.static_framework,
                                   { 'Debug' => :debug }, ['arm64'], Pod::Platform.new(platform_name, deployment_target), specs,
                                   [podfile.target_definitions.fetch('Host')])
    phases = pod_target.script_phases
    raise 'Full artifact guard duplicated or absent' unless phases.sum { |phase| phase[:script].scan('--installed-artifact ').length } == 1
    project = Xcodeproj::Project.new(host.join("#{names.join('-')}.xcodeproj"))
    native = project.new_target(:static_library, 'Fixture', platform_name, deployment_target)
    Pod::Installer::UserProjectIntegrator::TargetIntegrator.create_or_update_user_script_phases(phases, native)
    guard = native.shell_script_build_phases.find { |phase| phase.name == '[CP-User] Validate selected core artifacts' }
    copy = native.shell_script_build_phases.find { |phase| phase.name == '[CP-User] Install selected core capabilities' }
    raise 'Guard can be skipped with an up-to-date copy' unless guard && guard.always_out_of_date == '1' && Array(guard.output_paths).empty?
    raise 'Capabilities copy is not incremental' unless copy && copy.always_out_of_date.nil? && copy.input_paths == [manifest.realpath.to_s]
    raise 'Wrong manifest output' unless copy.output_paths == [expected_output]
    raise 'Duplicate manifest producer' unless native.shell_script_build_phases.flat_map { |phase| Array(phase.output_paths) } == copy.output_paths
    raise 'Wrong guard/copy execution order' unless native.build_phases.index(guard) < native.build_phases.index(copy) && native.build_phases.index(copy) < native.build_phases.index(native.source_build_phase)
    [guard.shell_script, copy.shell_script]
  end
end
