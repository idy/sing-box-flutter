# SPDX-License-Identifier: GPL-3.0-or-later
require 'cocoapods'
require 'tmpdir'
require 'fileutils'
require 'json'
require 'digest'
require 'open3'
require_relative '../../ios/Tests/podspec_phase_contract'

# CocoaPods evaluation and cached build guards; fixture bytes are not link proof.
podspec = File.expand_path('../sing_box_flutter.podspec', __dir__)
pod_root = File.dirname(podspec)
source_root = File.expand_path('../..', __dir__)
Dir.mktmpdir('sing box macos pod contract ') do |directory|
  host = Pathname.new(directory)
  FileUtils.mkdir_p(host.join('macos'))
  Pod::Config.instance.installation_root = host.join('macos')
  pubspec = host.join('pubspec.yaml')
  load_spec = lambda do
    spec = Pod::Specification.from_file(podspec)
    validator = Pod::Validator.new(spec, [], [])
    validator.quick = true
    validator.allow_warnings = true
    validator.ignore_public_only_results = true
    Pod::Config.instance.with_changes(:silent => true) { validator.validate }
    raise Pod::Informative, validator.results_message unless validator.validated?
    spec
  end
  rejected = lambda do
    begin
      load_spec.call
    rescue Pod::DSLError
      next
    end
    raise 'Invalid macOS selection/artifact unexpectedly accepted'
  end
  ["name: fixture\n", "name: fixture\nsing_box_flutter: {}\n",
   "name: fixture\nsing_box_flutter:\n  macos: {mode: invalid}\n",
   "name: fixture\nsing_box_flutter:\n  macos: {mode: ne, extra: true}\n",
   "name: fixture\nsing_box_flutter:\n  macos: null\n",
   "name: fixture\nsing_box_flutter:\n  macos: {mode: null}\n",
   "name: fixture\nsing_box_flutter:\n  macos: {}\n",
   "name: fixture\nsing_box_flutter:\n  macos: {mode: user_tun}\n"].each do |text|
    pubspec.write(text)
    rejected.call
  end

  base = host.join('.dart_tool/sing_box_flutter')
  framework = base.join('macos/Libbox.xcframework')
  slice = { 'LibraryIdentifier' => 'macos-arm64_x86_64', 'LibraryPath' => 'Libbox.framework',
            'SupportedArchitectures' => %w[arm64 x86_64], 'SupportedPlatform' => 'macos' }
  binary = framework.join(slice['LibraryIdentifier'], slice['LibraryPath'], 'Libbox')
  FileUtils.mkdir_p(binary.dirname)
  binary.write('fixture, not a linked binary')
  write_slices = ->(slices) { Xcodeproj::Plist.write_to_path({ 'AvailableLibraries' => slices }, framework.join('Info.plist')) }
  write_slices.call([slice])
  manifest = base.join('macos/core-capabilities.json')
  metadata = base.join('macos/native-build.json')
  receipt_path = base.join('native.json')
  tree_hashes = lambda do
    framework.glob('**/*').each_with_object({}) do |path, hashes|
      key = path.relative_path_from(framework).to_s
      if path.symlink?
        hashes[key] = Digest::SHA256.hexdigest('symlink:' + path.readlink.to_s)
      elsif path.file?
        hashes[key] = Digest::SHA256.file(path).hexdigest
      end
    end
  end
  receipt = nil
  save = -> { receipt_path.write(JSON.generate(receipt)) }
  configure = lambda do |block, mode|
    pubspec.write("name: fixture\nrelease_date: 2026-11-09\nsing_box_flutter:\n#{block}")
    code = <<~'PY'
      import json, sys
      from pathlib import Path
      sys.path.insert(0, str(Path(sys.argv[1]) / 'tool'))
      from build_core import build_identity, canonical_hash
      from core_features import canonicalize, resolve_features
      from native_common import load_pins
      pins = load_pins()
      selection = canonicalize({})
      macos = None if sys.argv[2] == 'implicit' else {'mode': sys.argv[2]}
      resolved = resolve_features(selection, 'macos')
      identity = build_identity(selection, [], pins, macos=macos)
      fingerprint = canonical_hash(identity)
      print(json.dumps({'capabilities': {'schema': 1, 'version': pins['sing_box_version'], 'fingerprint': fingerprint, 'features': resolved['features']},
                        'selection': selection,
                        'metadata': {'schema': 1, 'mode': 'selectable-core', 'target': 'macos', 'identity': identity, 'fingerprint': fingerprint, 'features': resolved['features'], 'tags': resolved['tags']}}))
    PY
    output, error, status = Open3.capture3('python3', '-I', '-c', code, source_root, mode)
    raise error unless status.success?
    fixture = JSON.parse(output)
    manifest.write(JSON.generate(fixture.fetch('capabilities')))
    metadata.write(JSON.generate(fixture.fetch('metadata')))
    receipt = fixture.fetch('capabilities').merge(
      'requested_features' => fixture.fetch('selection'),
      'pubspec_sha256' => Digest::SHA256.file(pubspec).hexdigest,
      'artifacts' => { 'macos' => {
        'sha256' => tree_hashes.call, 'path' => framework.relative_path_from(host).to_s,
        'capabilities' => manifest.relative_path_from(host).to_s,
        'metadata' => metadata.relative_path_from(host).to_s,
        'capabilities_sha256' => Digest::SHA256.file(manifest).hexdigest
      } }
    )
    receipt['macos'] = { 'mode' => mode } unless mode == 'implicit'
    save.call
    unless block.include?('core:')
      # Exercise the reported sequence with a genuine macOS-only receipt:
      # building macOS must leave the same host's iOS pod on legacy artifacts.
      ios = Pod::Specification.from_file(File.join(source_root, 'ios/sing_box_flutter.podspec'))
      raise 'macOS-only receipt selected iOS core' unless ios.subspec_by_name('sing_box_flutter/Core').consumer(:ios).vendored_frameworks == ['Frameworks/Libbox.xcframework']
      shared_ios = ios.subspec_by_name('sing_box_flutter/Shared').consumer(:ios)
      raise 'macOS-only receipt installed selected iOS guards' unless shared_ios.script_phases.empty? && shared_ios.resource_bundles['sing_box_flutter_capabilities'] == ['Resources/core-capabilities.json']
    end
    load_spec.call
  end

  selected = configure.call("  core: {}\n", 'implicit')
  SingBoxFlutterPodspecContract.installed_selection_contract(selected, host: host, source_root: source_root, platform: 'macos')
  raise 'macOS minimum must be 12.0' unless selected.deployment_target(:osx) == '12.0'
  plugin = selected.subspec_by_name('sing_box_flutter/Plugin').consumer(:osx)
  raise 'NE plugin links Go' unless plugin.dependencies.map(&:name).sort == ['FlutterMacOS', 'sing_box_flutter/Shared']
  provider = selected.subspec_by_name('sing_box_flutter/PacketTunnel').consumer(:osx)
  raise 'NE extension links Flutter' unless provider.dependencies.map(&:name).sort == ['sing_box_flutter/Core', 'sing_box_flutter/Shared']
  core = selected.subspec_by_name('sing_box_flutter/Core').consumer(:osx)
  raise 'Host-local static artifact must not be copied' unless core.vendored_frameworks.empty? && core.pod_target_xcconfig['FRAMEWORK_SEARCH_PATHS[sdk=macosx*]'].include?(framework.to_s)
  raise 'Native fingerprint flag absent' unless core.pod_target_xcconfig['OTHER_SWIFT_FLAGS'].include?('SINGBOX_SELECTABLE_CORE')
  %w[Shared PacketTunnel Plugin].each do |name|
    consumer = selected.subspec_by_name("sing_box_flutter/#{name}").consumer(:osx)
    accessor = Pod::Sandbox::FileAccessor.new(Pathname.new(pod_root), consumer)
    raise "CocoaPods did not discover #{name} shared sources" if accessor.source_files.empty?
    raise "#{name} duplicates canonical iOS source" unless accessor.source_files.all? { |path| path.realpath.to_s.start_with?(File.join(source_root, 'ios') + '/') }
  end

  shared = selected.subspec_by_name('sing_box_flutter/Shared').consumer(:osx)
  raise 'Shared links engine' unless shared.dependencies.empty?
  raise 'NE preflight source missing' unless Pod::Sandbox::FileAccessor.new(Pathname.new(pod_root), shared).source_files.any? { |path| path.basename.to_s == 'ConfigPreflight.swift' }
  raise 'Unused macOS staging setting retained' if provider.pod_target_xcconfig.key?('PODS_XCFRAMEWORKS_BUILD_DIR') || provider.user_target_xcconfig.to_s.include?('SingBoxFlutterPacketTunnelXCFrameworks')
  # Exercise actual PodTarget phase merging and CocoaPods' Xcode integration.
  # Shared must guard both native and Go-free targets; root phases do not inherit.
  phase_scripts = lambda do |spec, names|
    SingBoxFlutterPodspecContract.phase_scripts(spec, names, host: host, manifest: manifest,
      platform_name: :osx, deployment_target: '12.0',
      expected_output: '$(TARGET_BUILD_DIR)/sing_box_flutter_capabilities.bundle/Contents/Resources/core-capabilities.json')
  end
  guard_script, copy_script = phase_scripts.call(selected, %w[Plugin Shared])
  phase_scripts.call(selected, %w[Core PacketTunnel Shared])
  phase_scripts.call(selected, %w[Core Shared])
  raise 'Direct Core consumer bypasses guard' unless core.dependencies.map(&:name).include?('sing_box_flutter/Shared')
  target = host.join('build products')
  bundle = target.join('sing_box_flutter_capabilities.bundle')
  resources = bundle.join('Contents/Resources')
  FileUtils.mkdir_p(resources)
  Xcodeproj::Plist.write_to_path({ 'CFBundleIdentifier' => 'fixture.capabilities', 'CFBundlePackageType' => 'BNDL' }, bundle.join('Contents/Info.plist'))
  FileUtils.cp(File.join(pod_root, 'Resources/PrivacyInfo.xcprivacy'), resources)
  run = -> { system({ 'TARGET_BUILD_DIR' => target.to_s }, '/bin/sh', '-c', guard_script, out: File::NULL, err: File::NULL) }
  raise 'Selected artifact validation failed' unless run.call
  raise 'Selected manifest installation failed' unless system({ 'TARGET_BUILD_DIR' => target.to_s }, '/bin/sh', '-c', copy_script)
  raise 'Wrong selected capabilities bundled' unless resources.join('core-capabilities.json').read == manifest.read
  raise 'Manifest copied to macOS bundle root' if bundle.join('core-capabilities.json').exist?
  # Inspect realistic app and embedded extension layouts with Foundation, not a
  # flat directory that Bundle would accept as a non-macOS resource bundle.
  app = target.join('Fixture.app')
  extension = app.join('Contents/PlugIns/PacketTunnel.appex')
  [app, extension].each_with_index do |owner, index|
    FileUtils.mkdir_p(owner.join('Contents/Resources'))
    Xcodeproj::Plist.write_to_path({ 'CFBundleIdentifier' => "fixture.owner#{index}", 'CFBundlePackageType' => index.zero? ? 'APPL' : 'XPC!' }, owner.join('Contents/Info.plist'))
    FileUtils.cp_r(bundle, owner.join('Contents/Resources'))
  end
  probe = File.join(__dir__, 'check-built-bundles.swift')
  output, error, status = Open3.capture3('xcrun', 'swift', probe, app.to_s)
  raise error unless status.success?
  raise 'Foundation did not check both app and extension' unless output.scan('Capabilities OK:').length == 2 && output.scan(receipt['fingerprint']).length == 2
  app_resources = app.join('Contents/Resources/sing_box_flutter_capabilities.bundle')
  installed_manifest = app_resources.join('Contents/Resources/core-capabilities.json')
  FileUtils.mv(installed_manifest, app_resources.join('core-capabilities.json'))
  _output, error, status = Open3.capture3('xcrun', 'swift', probe, app.to_s)
  raise 'Foundation accepted root-only macOS manifest' if status.success? || !error.include?('Foundation cannot resolve')
  FileUtils.mv(app_resources.join('core-capabilities.json'), installed_manifest)
  # Even a valid JSON rewrite and self-consistent receipt must not replace the
  # capabilities hash pinned by pod install when the copy output is up to date.
  installed_manifest_bytes = manifest.read
  manifest.write(installed_manifest_bytes + "\n")
  receipt['artifacts']['macos']['capabilities_sha256'] = Digest::SHA256.file(manifest).hexdigest
  save.call
  raise 'Cached Pods accepted rewritten capabilities and hash' if run.call
  manifest.write(installed_manifest_bytes)
  receipt['artifacts']['macos']['capabilities_sha256'] = Digest::SHA256.file(manifest).hexdigest
  save.call
  sdk_phase = core.script_phases.first
  sdk_script = sdk_phase[:script] || sdk_phase['script']
  raise 'macOS SDK rejected' unless system({ 'PLATFORM_NAME' => 'macosx' }, '/bin/sh', '-c', sdk_script)
  raise 'iOS SDK accepted' if system({ 'PLATFORM_NAME' => 'iphoneos' }, '/bin/sh', '-c', sdk_script, out: File::NULL, err: File::NULL)

  original_binary = binary.read
  binary.write('changed after pod install')
  receipt['artifacts']['macos']['sha256'] = tree_hashes.call
  save.call
  raise 'Cached Pods accepted rewritten artifact and hashes' if run.call
  binary.write(original_binary)
  receipt['artifacts']['macos']['sha256'] = tree_hashes.call
  save.call
  original_metadata = metadata.read
  invalid_metadata = JSON.parse(original_metadata)
  invalid_metadata['identity']['mobile_sources'].transform_values! { '0' * 64 }
  metadata.write(JSON.generate(invalid_metadata))
  rejected.call
  raise 'Cached Pods accepted stale native recipe' if run.call
  metadata.write(original_metadata)

  write_slices.call([slice.merge('SupportedArchitectures' => ['arm64'])])
  receipt['artifacts']['macos']['sha256'] = tree_hashes.call
  save.call
  rejected.call
  write_slices.call([slice, slice.merge('LibraryIdentifier' => 'duplicate')])
  receipt['artifacts']['macos']['sha256'] = tree_hashes.call
  save.call
  rejected.call
  write_slices.call([slice.merge('SupportedPlatform' => 'ios')])
  receipt['artifacts']['macos']['sha256'] = tree_hashes.call
  save.call
  rejected.call
  write_slices.call([slice])
  receipt['artifacts']['macos']['sha256'] = tree_hashes.call
  save.call

  selected = configure.call("  macos: {}\n", 'ne')
  raise 'Empty macos selection did not default to NE' unless selected.subspecs.map(&:name).include?('sing_box_flutter/PacketTunnel')
  configure.call("  core: {}\n  macos: {mode: ne}\n", 'ne')
  user_tun = configure.call("  macos: {mode: user_tun}\n", 'user_tun')
  raise 'Mode switch accepted by cached Pods' if run.call
  plugin = user_tun.subspec_by_name('sing_box_flutter/Plugin').consumer(:osx)
  raise 'Standalone plugin does not link Core' unless plugin.dependencies.map(&:name).sort == ['FlutterMacOS', 'sing_box_flutter/Core', 'sing_box_flutter/Shared']
  raise 'user_tun compiles NE extension sources' if user_tun.subspecs.map(&:name).include?('sing_box_flutter/PacketTunnel')
  raise 'user_tun links NetworkExtension' if user_tun.subspec_by_name('sing_box_flutter/Core').consumer(:osx).frameworks.include?('NetworkExtension')
  standalone_shared = user_tun.subspec_by_name('sing_box_flutter/Shared').consumer(:osx)
  standalone_sources = Pod::Sandbox::FileAccessor.new(Pathname.new(pod_root), standalone_shared).source_files
  raise 'user_tun still compiles NE preflight' if standalone_sources.any? { |path| path.basename.to_s == 'ConfigPreflight.swift' }
  raise 'user_tun lost common capabilities loader' unless standalone_sources.any? { |path| path.basename.to_s == 'CoreCapabilities.swift' }
  phase_scripts.call(user_tun, %w[Plugin Core Shared])
  sources = Pod::Sandbox::FileAccessor.new(Pathname.new(pod_root), plugin).source_files.map(&:to_s)
  raise 'Wrong standalone plugin sources' unless sources.any? { |file| file.end_with?('/UserTun/SingBoxFlutterPlugin.swift') } && sources.any? { |file| file.end_with?('/NativeCore.swift') } && sources.all? { |file| file.include?('/UserTun/') || file.end_with?('/NativeCore.swift') }
  receipt['macos'] = { 'mode' => 'ne' }
  save.call
  rejected.call
  receipt['macos'] = { 'mode' => 'user_tun' }
  receipt['artifacts']['macos']['path'] = '../Libbox.xcframework'
  save.call
  rejected.call
  receipt['artifacts']['macos']['path'] = framework.relative_path_from(host).to_s
  File.symlink('macos', base.join('alias'))
  receipt['artifacts']['macos']['path'] = '.dart_tool/sing_box_flutter/alias/Libbox.xcframework'
  save.call
  rejected.call
  receipt['artifacts']['macos']['path'] = framework.relative_path_from(host).to_s
  save.call
  File.symlink('/etc/hosts', framework.join('escape'))
  receipt['artifacts']['macos']['sha256'] = tree_hashes.call
  save.call
  rejected.call
  puts 'macOS podspec contract passed: source reuse, mode isolation, universal SDK slices, receipt integrity and cached-Pods guards.'
end
