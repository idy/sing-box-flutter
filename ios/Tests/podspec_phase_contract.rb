# SPDX-License-Identifier: GPL-3.0-or-later
require 'cocoapods'

# Exercise actual CocoaPods target merging and Xcode phase integration for each
# Apple platform. The callers retain their independent expected output layouts.
module SingBoxFlutterPodspecContract
  # Use CocoaPods' actual serialization and checksum generation. A hand-written
  # marker fixture alone would miss Ruby/Python or sandbox lock incompatibility.
  def self.install_spec(spec, host:, platform:)
    sandbox = Pod::Sandbox.new(host.join(platform, 'Pods'))
    sandbox.prepare
    sandbox.store_podspec(spec.name, spec, true, true)
    installed = sandbox.specification(spec.name)
    lock = Pod::Lockfile.generate(Pod::Podfile.new, [installed], {})
    lock.write_to_disk(host.join(platform, 'Podfile.lock'))
    lock.write_to_disk(sandbox.manifest_path)
    sandbox.manifest_path
  end

  def self.refresh_receipt(host:, source_root:)
    code = <<~'PY'
      import json, sys
      from pathlib import Path
      sys.path.insert(0, str(Path(sys.argv[1]) / 'tool'))
      from build_core import RECEIPT, update_receipt
      host = Path(sys.argv[2])
      receipt = json.loads((host / RECEIPT).read_text())
      capabilities = {key: receipt[key] for key in ('schema', 'version', 'fingerprint', 'features', 'macos') if key in receipt}
      update_receipt(host, capabilities, receipt['requested_features'], {})
    PY
    _output, error, status = Open3.capture3('python3', '-I', '-c', code, source_root, host.to_s)
    raise error unless status.success?
  end

  def self.installed_selection_contract(spec, host:, source_root:, platform:)
    manifest = install_spec(spec, host: host, platform: platform)
    locked = manifest.read
    refresh_receipt(host: host, source_root: source_root)
    raise 'Identical selected core unnecessarily refreshed Pods' unless manifest.file? && manifest.read == locked
    # CocoaPods writes Local Podspecs before it finishes generating the project.
    # The new marker is not installed until the sandbox checksum is published.
    podspec = manifest.dirname.join('Local Podspecs/sing_box_flutter.podspec.json')
    podspec.write(podspec.read + "\n")
    refresh_receipt(host: host, source_root: source_root)
    raise 'Uncommitted podspec accepted as installed' if manifest.exist?
    raise 'Podfile.lock changed during refresh' unless host.join(platform, 'Podfile.lock').read == locked
  end

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
