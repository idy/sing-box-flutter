# SPDX-License-Identifier: GPL-3.0-or-later
require 'cocoapods'
require 'tmpdir'
require 'fileutils'
require 'json'
require 'digest'
require 'open3'
require_relative 'podspec_phase_contract'

# Exercises CocoaPods evaluation only. Fixture binaries are not integration proof.
podspec = File.expand_path('../sing_box_flutter.podspec', __dir__)
Dir.mktmpdir('sing box pod contract ') do |directory|
  host = Pathname.new(directory)
  FileUtils.mkdir_p(host.join('ios'))
  Pod::Config.instance.installation_root = host.join('ios')
  pubspec = host.join('pubspec.yaml')
  pubspec.write("name: fixture\n")
  load_spec = lambda do
    spec = Pod::Specification.from_file(podspec)
    # Match the quick validator used when CocoaPods fetches an external path pod.
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
    raise 'Invalid receipt unexpectedly accepted'
  end
  legacy = load_spec.call
  raise 'Plugin links core' unless legacy.subspec_by_name('sing_box_flutter/Plugin').dependencies.map(&:name).sort == ['Flutter', 'sing_box_flutter/Shared']
  raise 'Legacy framework absent' unless legacy.subspec_by_name('sing_box_flutter/Core').consumer(:ios).vendored_frameworks == ['Frameworks/Libbox.xcframework']
  legacy_shared = legacy.subspec_by_name('sing_box_flutter/Shared').consumer(:ios)
  raise 'Legacy resources changed' unless legacy_shared.resource_bundles == {
    'sing_box_flutter_capabilities' => ['Resources/core-capabilities.json'],
    'sing_box_flutter_privacy' => ['sing_box_flutter/Sources/sing_box_flutter/PrivacyInfo.xcprivacy']
  } && legacy_shared.script_phases.empty?
  raise 'Legacy extension staging removed' unless legacy.subspec_by_name('sing_box_flutter/PacketTunnel').consumer(:ios).pod_target_xcconfig.key?('PODS_XCFRAMEWORKS_BUILD_DIR')
  pubspec.write("name: fixture\nrelease_date: 2026-11-09\ngenerated_at: 2026-11-09T12:00:00Z\n")
  load_spec.call # Date-like custom metadata is valid in Dart/Flutter.
  pubspec.write("name: fixture\ncustom: !ruby/object:Object {}\n")
  rejected.call # Keep arbitrary YAML object construction forbidden.
  pubspec.write("name: fixture\nsing_box_flutter:\n  core: {}\n")
  rejected.call

  base = host.join('.dart_tool/sing_box_flutter')
  framework = base.join('apple/Libbox.xcframework')
  slices = [
    { 'LibraryIdentifier' => 'ios-arm64', 'LibraryPath' => 'Libbox.framework', 'SupportedArchitectures' => ['arm64'], 'SupportedPlatform' => 'ios' },
    { 'LibraryIdentifier' => 'ios-arm64_x86_64-simulator', 'LibraryPath' => 'Libbox.framework', 'SupportedArchitectures' => ['arm64', 'x86_64'], 'SupportedPlatform' => 'ios', 'SupportedPlatformVariant' => 'simulator' }
  ]
  slices.each do |slice|
    path = framework.join(slice['LibraryIdentifier'], slice['LibraryPath'])
    FileUtils.mkdir_p(path)
    path.join('Libbox').write('fixture, not a linked binary')
  end
  Xcodeproj::Plist.write_to_path({ 'AvailableLibraries' => slices }, framework.join('Info.plist'))
  source_root = File.expand_path('../..', __dir__)
  fixture_code = <<~'PY'
    import json, sys
    from pathlib import Path
    sys.path.insert(0, str(Path(sys.argv[1]) / 'tool'))
    from build_core import build_identity, canonical_hash
    from core_features import canonicalize, resolve_features
    from native_common import load_pins
    pins = load_pins()
    selection = canonicalize({})
    resolved = resolve_features(selection, 'apple')
    macos = json.loads(sys.argv[2]) if len(sys.argv) > 2 else None
    identity = build_identity(selection, [], pins, macos=macos)
    fingerprint = canonical_hash(identity)
    print(json.dumps({'capabilities': {'schema': 1, 'version': pins['sing_box_version'], 'fingerprint': fingerprint, 'features': resolved['features']},
                      'selection': selection,
                      'metadata': {'schema': 1, 'mode': 'selectable-core', 'target': 'apple', 'identity': identity, 'fingerprint': fingerprint, 'features': resolved['features'], 'tags': resolved['tags']}}))
  PY
  fixture_json, fixture_error, fixture_status = Open3.capture3('python3', '-I', '-c', fixture_code, source_root)
  raise fixture_error unless fixture_status.success?
  fixture = JSON.parse(fixture_json)
  capabilities = fixture.fetch('capabilities')
  metadata = base.join('apple/native-build.json')
  metadata.write(JSON.generate(fixture.fetch('metadata')))
  manifest = base.join('apple/core-capabilities.json')
  manifest.write(JSON.generate(capabilities))
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
  receipt = capabilities.merge(
    'requested_features' => fixture.fetch('selection'),
    'pubspec_sha256' => Digest::SHA256.file(pubspec).hexdigest,
    'artifacts' => { 'apple' => {
      'sha256' => tree_hashes.call,
      'path' => framework.relative_path_from(host).to_s,
      'capabilities' => manifest.relative_path_from(host).to_s,
      'metadata' => metadata.relative_path_from(host).to_s,
      'capabilities_sha256' => Digest::SHA256.file(manifest).hexdigest
    } }
  )
  receipt_path = base.join('native.json')
  save_receipt = -> { receipt_path.write(JSON.generate(receipt)) }
  save_receipt.call
  selected = load_spec.call
  # A host's macOS selection must not change the iOS source/link split. Empty
  # macOS mappings and macOS-only blocks use the same canonical core defaults.
  original_pubspec = pubspec.read
  original_metadata = metadata.read
  original_manifest = manifest.read
  original_receipt = JSON.generate(receipt)
  [
    ["  core: {}\n  macos: {mode: ne}\n", 'ne'],
    ["  macos: {}\n", 'ne'],
    ["  macos: {mode: user_tun}\n", 'user_tun']
  ].each do |block, mode|
    pubspec.write("name: fixture\nsing_box_flutter:\n#{block}")
    output, error, status = Open3.capture3('python3', '-I', '-c', fixture_code, source_root, JSON.generate({ 'mode' => mode }))
    raise error unless status.success?
    cross_platform_fixture = JSON.parse(output)
    metadata.write(JSON.generate(cross_platform_fixture.fetch('metadata')))
    manifest.write(JSON.generate(cross_platform_fixture.fetch('capabilities')))
    receipt.merge!(cross_platform_fixture.fetch('capabilities'))
    receipt['macos'] = { 'mode' => mode }
    receipt['pubspec_sha256'] = Digest::SHA256.file(pubspec).hexdigest
    receipt['artifacts']['apple']['capabilities_sha256'] = Digest::SHA256.file(manifest).hexdigest
    save_receipt.call
    cross_platform = load_spec.call
    raise 'macOS mode changed iOS plugin linkage' unless cross_platform.subspec_by_name('sing_box_flutter/Plugin').dependencies.map(&:name).sort == ['Flutter', 'sing_box_flutter/Shared']
  end
  pubspec.write(original_pubspec)
  metadata.write(original_metadata)
  manifest.write(original_manifest)
  receipt = JSON.parse(original_receipt)
  save_receipt.call
  core = selected.subspec_by_name('sing_box_flutter/Core').consumer(:ios)
  raise 'External framework lost or re-copied' unless core.vendored_frameworks.empty? && core.pod_target_xcconfig['FRAMEWORK_SEARCH_PATHS[sdk=iphoneos*]'].include?(framework.to_s)
  raise 'Missing fingerprint flag' unless core.pod_target_xcconfig['OTHER_SWIFT_FLAGS'].include?('SINGBOX_SELECTABLE_CORE')
  shared = selected.subspec_by_name('sing_box_flutter/Shared').consumer(:ios)
  raise 'Shared links engine' unless shared.dependencies.empty? && !shared.pod_target_xcconfig.to_s.include?('Libbox')
  raise 'Fallback bundled in selected build' if shared.resource_bundles.values.flatten.any? { |p| p.end_with?('core-capabilities.json') }
  # Use CocoaPods' real target merge and Xcode integrator, including the Go-free
  # controller. Root script phases do not inherit into subspec consumers.
  phase_scripts = lambda do |names|
    SingBoxFlutterPodspecContract.phase_scripts(selected, names, host: host, manifest: manifest,
      platform_name: :ios, deployment_target: '15.0',
      expected_output: '$(TARGET_BUILD_DIR)/sing_box_flutter_capabilities.bundle/core-capabilities.json')
  end
  guard_script, copy_script = phase_scripts.call(%w[Plugin Shared])
  phase_scripts.call(%w[Core PacketTunnel Shared])
  phase_scripts.call(%w[Core Shared])
  raise 'Direct Core consumer bypasses guard' unless core.dependencies.map(&:name).include?('sing_box_flutter/Shared')
  target = host.join('build products')
  FileUtils.mkdir_p(target.join('sing_box_flutter_capabilities.bundle'))
  run_phase = -> { system({ 'TARGET_BUILD_DIR' => target.to_s }, '/bin/sh', '-c', guard_script, out: File::NULL, err: File::NULL) }
  raise 'Artifact guard failed' unless run_phase.call
  raise 'Manifest copy failed' unless system({ 'TARGET_BUILD_DIR' => target.to_s }, '/bin/sh', '-c', copy_script)
  raise 'Wrong bundled manifest' unless target.join('sing_box_flutter_capabilities.bundle/core-capabilities.json').read == manifest.read
  # A valid JSON rewrite with updated receipt hashes must still fail the hash
  # pinned by pod install, even if Xcode would skip the copy's existing output.
  manifest.write(manifest.read + "\n")
  receipt['artifacts']['apple']['capabilities_sha256'] = Digest::SHA256.file(manifest).hexdigest
  save_receipt.call
  raise 'Rewritten capabilities accepted by cached Pods' if run_phase.call
  manifest.write(original_manifest)
  receipt['artifacts']['apple']['capabilities_sha256'] = Digest::SHA256.file(manifest).hexdigest
  save_receipt.call
  # Xcode links the framework chosen at pod install. A newer valid receipt for
  # another artifact of the same profile still needs pod install.
  rebuilt = base.join('apple-rebuilt/Libbox.xcframework')
  FileUtils.mkdir_p(rebuilt.dirname)
  FileUtils.cp_r(framework, rebuilt)
  installed_path = receipt['artifacts']['apple']['path']
  receipt['artifacts']['apple']['path'] = rebuilt.relative_path_from(host).to_s
  save_receipt.call
  raise 'Receipt for another framework accepted by cached Pods' if run_phase.call
  receipt['artifacts']['apple']['path'] = installed_path
  save_receipt.call
  # Rewriting the installed framework and its receipt hashes is not the pod-installed artifact.
  installed_binary = framework.join(slices.first['LibraryIdentifier'], slices.first['LibraryPath'], 'Libbox')
  installed_bytes = installed_binary.read
  installed_binary.write('changed after pod install')
  receipt['artifacts']['apple']['sha256'] = tree_hashes.call
  save_receipt.call
  raise 'Framework changed after pod install accepted' if run_phase.call
  installed_binary.write(installed_bytes)
  receipt['artifacts']['apple']['sha256'] = tree_hashes.call
  save_receipt.call
  raise 'Installed framework rejected' unless run_phase.call
  # Native add-to-app Podfiles can explicitly identify their separate module.
  native_host = host.join('native-ios-host')
  FileUtils.mkdir_p(native_host)
  previous_override = ENV['SINGBOX_HOST_PROJECT']
  begin
    ENV['SINGBOX_HOST_PROJECT'] = host.to_s
    Pod::Config.instance.installation_root = native_host
    module_spec = load_spec.call.subspec_by_name('sing_box_flutter/Core').consumer(:ios)
    raise 'Module override ignored' unless module_spec.pod_target_xcconfig['OTHER_SWIFT_FLAGS'].include?('SINGBOX_SELECTABLE_CORE')
  ensure
    ENV['SINGBOX_HOST_PROJECT'] = previous_override
    Pod::Config.instance.installation_root = host.join('ios')
  end
  # Internally consistent binaries must not accept an edited current recipe.
  original_metadata = metadata.read
  stale_metadata = JSON.parse(original_metadata)
  key = stale_metadata['identity']['mobile_sources'].keys.first
  stale_metadata['identity']['mobile_sources'][key] = '0' * 64
  metadata.write(JSON.generate(stale_metadata))
  rejected.call
  raise 'Cached phase accepted stale recipe' if run_phase.call
  metadata.write(original_metadata)
  # Old Pods may keep pointing at the previous profile after a successful core build.
  current_fingerprint = receipt['fingerprint']
  receipt['fingerprint'] = 'b' * 64
  save_receipt.call
  raise 'Stale CocoaPods profile accepted' if run_phase.call
  receipt['fingerprint'] = current_fingerprint
  save_receipt.call
  pubspec.write(pubspec.read + "# a profile edit during app build\n")
  raise 'Changed pubspec accepted by cached Pods' if run_phase.call
  pubspec.write("name: fixture\nsing_box_flutter:\n  core: {}\n")

  # A device-only artifact installs, but its build phase rejects simulator use.
  Xcodeproj::Plist.write_to_path({ 'AvailableLibraries' => [slices.first] }, framework.join('Info.plist'))
  receipt['artifacts']['apple']['sha256'] = tree_hashes.call
  save_receipt.call
  device_only = load_spec.call.subspec_by_name('sing_box_flutter/Core').consumer(:ios)
  sdk_phase = device_only.script_phases.first
  sdk_script = sdk_phase[:script] || sdk_phase['script']
  raise 'Device-only SDK check failed' unless system({ 'PLATFORM_NAME' => 'iphoneos' }, '/bin/sh', '-c', sdk_script)
  raise 'Missing simulator slice accepted' if system({ 'PLATFORM_NAME' => 'iphonesimulator' }, '/bin/sh', '-c', sdk_script, out: File::NULL, err: File::NULL)
  Xcodeproj::Plist.write_to_path({ 'AvailableLibraries' => slices }, framework.join('Info.plist'))
  save_receipt.call
  rejected.call # Artifact tree hash no longer matches.
  receipt['artifacts']['apple']['sha256'] = tree_hashes.call
  save_receipt.call
  load_spec.call
  File.symlink('apple', base.join('alias'))
  receipt['artifacts']['apple']['path'] = '.dart_tool/sing_box_flutter/alias/Libbox.xcframework'
  save_receipt.call
  rejected.call
  receipt['artifacts']['apple']['path'] = '.dart_tool/sing_box_flutter/apple/../apple/Libbox.xcframework'
  save_receipt.call
  rejected.call
  receipt['artifacts']['apple']['path'] = framework.relative_path_from(host).to_s
  receipt['pubspec_sha256'] = '0' * 64
  save_receipt.call
  rejected.call
  receipt['pubspec_sha256'] = Digest::SHA256.file(pubspec).hexdigest
  receipt['artifacts']['apple']['path'] = '/tmp/Libbox.xcframework'
  save_receipt.call
  rejected.call
  receipt['artifacts']['apple']['path'] = '.dart_tool/missing/Libbox.xcframework'
  save_receipt.call
  rejected.call
  receipt['artifacts']['apple']['path'] = framework.relative_path_from(host).to_s
  receipt['artifacts']['apple']['capabilities_sha256'] = '0' * 64
  save_receipt.call
  rejected.call
  receipt_path.write('{broken')
  rejected.call
  # An obsolete selected receipt must not block an intentional return to legacy.
  ["name: fixture\n", "name: fixture\nsing_box_flutter: {}\n"].each do |text|
    pubspec.write(text)
    standard = load_spec.call.subspec_by_name('sing_box_flutter/Core').consumer(:ios)
    raise 'Legacy transition still uses selected receipt' unless standard.vendored_frameworks == ['Frameworks/Libbox.xcframework']
  end
  puts 'Podspec contract passed: isolation, selected slices, manifest copy and fail-closed receipt handling.'
end
