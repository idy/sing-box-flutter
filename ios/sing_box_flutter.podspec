require 'json'
require 'pathname'
require 'digest'
require 'yaml'
require 'shellwords'
require 'find'
require 'date'
require 'open3'

# Read project-local artifacts without copying anything into the plugin/pub cache.
# Flutter invokes CocoaPods with installation_root set to HOST/ios.
explicit_host = ENV['SINGBOX_HOST_PROJECT']
host_root = if explicit_host && !explicit_host.empty?
              supplied = Pathname.new(explicit_host)
              raise 'SINGBOX_HOST_PROJECT must be an absolute Flutter project containing pubspec.yaml.' unless supplied.absolute? && supplied.join('pubspec.yaml').file?
              supplied.realpath
            elsif defined?(Pod::Config)
              Pod::Config.instance.installation_root.parent.expand_path
            end
receipt_path = host_root && host_root.join('.dart_tool/sing_box_flutter/native.json')
configured = false
if host_root && host_root.join('pubspec.yaml').file?
  begin
    pubspec = YAML.safe_load(host_root.join('pubspec.yaml').read, permitted_classes: [Date, Time], aliases: true)
    raise 'Host pubspec.yaml must be a mapping.' unless pubspec.is_a?(Hash)
    if pubspec.key?('sing_box_flutter')
      config = pubspec['sing_box_flutter']
      raise 'Invalid sing_box_flutter configuration.' unless config.is_a?(Hash) && (config.keys - ['core']).empty?
      configured = config.key?('core')
    end
  rescue Psych::Exception
    raise 'Invalid host pubspec.yaml. Resolve it before pod install.'
  end
end
# Removing the selection deliberately returns to the standard distribution;
# ignored old cache receipts must not prevent that transition.
selectable = configured && receipt_path && (receipt_path.exist? || receipt_path.symlink?)
if configured && !selectable
  raise 'Build the project core with dart run sing_box_flutter build before pod install.'
end
framework_path = 'Frameworks/Libbox.xcframework'
capabilities_path = 'Resources/core-capabilities.json'

if selectable
  begin
    %w[.dart_tool .dart_tool/sing_box_flutter .dart_tool/sing_box_flutter/native.json].each do |part|
      raise 'Receipt path must not be a symlink' if host_root.join(part).symlink?
    end
    receipt = JSON.parse(receipt_path.read)
    raise 'Stale host pubspec' unless receipt['pubspec_sha256'] == Digest::SHA256.file(host_root.join('pubspec.yaml')).hexdigest
    groups = %w[inbounds outbounds endpoints dns transports tls api services certificate_providers]
    fingerprint = receipt.fetch('fingerprint')
    raise 'Invalid receipt schema or identity' unless receipt['schema'] == 1 && fingerprint.is_a?(String) && fingerprint.match?(/\A[0-9a-f]{64}\z/) && receipt['version'].is_a?(String) && !receipt['version'].empty?
    features = receipt.fetch('features')
    raise 'Invalid receipt features' unless features.is_a?(Hash) && features.keys.sort == groups.sort && features.values.all? { |v| v.is_a?(Array) && v.all? { |x| x.is_a?(String) && !x.empty? } && v.uniq == v }
    apple = receipt.fetch('artifacts').fetch('apple')
    recipe_checker = File.expand_path('../tool/core_recipe.py', __dir__)
    artifact_digest = nil
    recipe_python = [ENV['SINGBOX_PYTHON'], 'python3.14', 'python3.13', 'python3.12', 'python3.11', 'python3', 'python'].compact.uniq.find do |python|
      begin
        output, _error, status = Open3.capture3(python, '-I', recipe_checker, '--project-dir', host_root.to_s, '--target', 'apple', '--print-artifact-digest')
        artifact_digest = output.strip if status.success?
        status.success?
      rescue SystemCallError
        false
      end
    end
    raise 'Stale selected core recipe or missing Python 3; set SINGBOX_PYTHON and rebuild.' unless recipe_python && artifact_digest.match?(/\A[0-9a-f]{64}\z/)
    resolve = lambda do |relative|
      raise 'Artifact paths must be project relative' unless relative.is_a?(String) && !relative.empty? && !Pathname.new(relative).absolute? && !relative.include?('\\')
      parts = relative.split('/')
      raise 'Artifact must be inside the project native build directory' unless parts.take(2) == %w[.dart_tool sing_box_flutter] && parts.none? { |part| ['', '.', '..'].include?(part) }
      path = host_root
      parts.each do |part|
        path = path.join(part)
        raise 'Artifact path contains a symlink' if path.symlink?
      end
      prefix = host_root.join('.dart_tool/sing_box_flutter').realpath.to_s + File::SEPARATOR
      raise 'Artifact escapes native build directory' unless path.exist? && path.realpath.to_s.start_with?(prefix)
      path.realpath
    end
    framework = resolve.call(apple.fetch('path'))
    capabilities = resolve.call(apple.fetch('capabilities'))
    recipe_metadata = resolve.call(apple.fetch('metadata'))
    package_root = File.expand_path('..', __dir__)
    recipe_input_files = [receipt_path.to_s, host_root.join('pubspec.yaml').to_s, recipe_metadata.to_s,
                          File.join(package_root, 'native-core.json'), File.join(package_root, 'native/mobile'),
                          File.join(package_root, 'native/wgc'), File.join(package_root, 'tool')]
    raise 'Missing Apple XCFramework' unless framework.directory? && framework.basename.to_s == 'Libbox.xcframework' && framework.join('Info.plist').file?
    raise 'Missing core-capabilities.json' unless capabilities.file? && capabilities.basename.to_s == 'core-capabilities.json'
    hashes = {}
    Find.find(framework.to_s) do |entry|
      path = Pathname.new(entry)
      key = path.relative_path_from(framework).to_s
      if path.symlink?
        raise 'Framework symlink escapes artifact' unless path.realpath.to_s.start_with?(framework.to_s + '/')
        hashes[key] = Digest::SHA256.hexdigest('symlink:' + path.readlink.to_s)
      elsif path.file?
        hashes[key] = Digest::SHA256.file(path).hexdigest
      end
    end
    raise 'Modified framework' unless apple['sha256'].is_a?(Hash) && !hashes.empty? && apple['sha256'] == hashes
    capabilities_sha256 = Digest::SHA256.file(capabilities).hexdigest
    raise 'Modified capabilities' unless apple['capabilities_sha256'] == capabilities_sha256
    manifest = JSON.parse(capabilities.read)
    raise 'Receipt and capabilities disagree' unless manifest['schema'] == 1 && manifest['version'] == receipt['version'] && manifest['fingerprint'] == fingerprint && manifest['features'] == features
    # CocoaPods' file accessor cannot glob files outside the plugin root. Link
    # the host-local static slices directly, without touching the plugin cache.
    libraries = Xcodeproj::Plist.read_from_path(framework.join('Info.plist'))['AvailableLibraries']
    raise 'Invalid XCFramework library list' unless libraries.is_a?(Array)
    slice_paths = {}
    { 'iphoneos' => nil, 'iphonesimulator' => 'simulator' }.each do |sdk, variant|
      candidates = libraries.select { |lib| lib['SupportedPlatform'] == 'ios' && lib['SupportedPlatformVariant'] == variant }
      next if candidates.empty?
      raise 'Ambiguous iOS slice' unless candidates.length == 1
      lib = candidates.first
      raise 'Missing arm64 slice' unless lib['SupportedArchitectures'].is_a?(Array) && lib['SupportedArchitectures'].include?('arm64')
      slice = framework.join(lib.fetch('LibraryIdentifier'), lib.fetch('LibraryPath')).cleanpath
      raise 'Invalid framework slice' unless slice.directory? && slice.realpath.to_s.start_with?(framework.to_s + '/') && slice.basename.to_s == 'Libbox.framework' && slice.join('Libbox').file?
      slice_paths[sdk] = slice.dirname.to_s
    end
    raise 'No supported iOS slices' if slice_paths.empty?
    framework_path = framework.to_s
    capabilities_path = capabilities.to_s
  rescue StandardError
    # Do not echo file contents or parser diagnostics from a malformed receipt.
    raise 'Invalid sing_box_flutter project Apple artifacts. Rebuild the native core for this host before running pod install.'
  end
end

profile_guard = if selectable
                  <<-SCRIPT
set -eu
# Xcode links the framework chosen at pod install; reject a receipt naming another.
#{Shellwords.escape(recipe_python)} -I #{Shellwords.escape(recipe_checker)} --project-dir #{Shellwords.escape(host_root.to_s)} --target apple --installed-artifact #{Shellwords.escape(apple.fetch('path'))} --installed-sha256 #{Shellwords.escape(artifact_digest)}
current_fingerprint=$(/usr/bin/plutil -extract fingerprint raw -o - #{Shellwords.escape(receipt_path.to_s)})
current_pubspec_hash=$(/usr/bin/shasum -a 256 #{Shellwords.escape(host_root.join('pubspec.yaml').to_s)})
current_pubspec_hash=${current_pubspec_hash%% *}
if [ "$current_fingerprint" != #{Shellwords.escape(fingerprint)} ] || [ "$current_pubspec_hash" != #{Shellwords.escape(receipt['pubspec_sha256'])} ]; then
  printf 'Selected core profile changed. Rebuild the native core and run pod install before building this app.\\n' >&2
  exit 1
fi
                  SCRIPT
                end

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
      shared.script_phase = {
        :name => 'Install selected core capabilities',
        :execution_position => :before_compile,
        :input_files => [capabilities_path, *recipe_input_files],
        :output_files => ['$(TARGET_BUILD_DIR)/sing_box_flutter_capabilities.bundle/core-capabilities.json'],
        :script => profile_guard + <<-SCRIPT
set -eu
source_manifest=#{Shellwords.escape(capabilities_path)}
actual_hash=$(/usr/bin/shasum -a 256 "$source_manifest")
actual_hash=${actual_hash%% *}
if [ "$actual_hash" != #{Shellwords.escape(capabilities_sha256)} ]; then
  printf 'Selected core capabilities changed; rebuild the native core and run pod install.\\n' >&2
  exit 1
fi
/usr/bin/ditto "$source_manifest" "${TARGET_BUILD_DIR}/sing_box_flutter_capabilities.bundle/core-capabilities.json"
        SCRIPT
      }
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
      core.script_phase = {
        :name => 'Check selected core SDK slice',
        :execution_position => :before_compile,
        :input_files => recipe_input_files,
        :script => profile_guard + <<-SCRIPT
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
