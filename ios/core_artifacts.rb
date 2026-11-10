# SPDX-License-Identifier: GPL-3.0-or-later
require 'json'
require 'pathname'
require 'digest'
require 'yaml'
require 'shellwords'
require 'date'
require 'open3'

# Both Apple podspecs use this resolver so receipt and cached-Pods checks stay
# identical. No host artifact is copied into the shared plugin/pub cache.
class SingBoxFlutterArtifacts
  attr_reader :selectable, :framework_path, :capabilities_path, :recipe_input_files,
              :capabilities_sha256, :slice_paths, :mode

  def initialize(platform:, pod_root:)
    @platform = platform
    target = platform == :macos ? 'macos' : 'apple'
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
    requested_macos = nil
    if host_root && host_root.join('pubspec.yaml').file?
      begin
        pubspec = YAML.safe_load(host_root.join('pubspec.yaml').read, permitted_classes: [Date, Time], aliases: true)
        raise 'Host pubspec.yaml must be a mapping.' unless pubspec.is_a?(Hash)
        if pubspec.key?('sing_box_flutter')
          config = pubspec['sing_box_flutter']
          raise 'Invalid sing_box_flutter configuration.' unless config.is_a?(Hash) && (config.keys - %w[core macos]).empty?
          configured = config.key?('core') || (platform == :macos && config.key?('macos'))
          if config.key?('macos')
            requested_macos = config['macos']
            raise 'sing_box_flutter.macos accepts only mode: ne or user_tun.' unless requested_macos.is_a?(Hash) && (requested_macos.keys - ['mode']).empty? && %w[ne user_tun].include?(requested_macos.fetch('mode', 'ne'))
            requested_macos = { 'mode' => requested_macos.fetch('mode', 'ne') }
          end
        end
      rescue Psych::Exception
        raise 'Invalid host pubspec.yaml. Resolve it before pod install.'
      end
    end
    @mode = requested_macos ? requested_macos['mode'] : 'ne'
    if platform == :macos && !configured
      raise 'macOS requires a sing_box_flutter.core or macos block in the host pubspec.yaml. Build it with dart run sing_box_flutter build --target macos before pod install.'
    end
    # Removing an iOS selection deliberately returns to its legacy distribution.
    @selectable = configured && receipt_path && (receipt_path.exist? || receipt_path.symlink?)
    if configured && !@selectable
      raise "Build the project core with dart run sing_box_flutter build --target #{target} before pod install."
    end
    @framework_path = 'Frameworks/Libbox.xcframework'
    @capabilities_path = 'Resources/core-capabilities.json'
    return unless @selectable

    begin
      %w[.dart_tool .dart_tool/sing_box_flutter .dart_tool/sing_box_flutter/native.json].each do |part|
        raise 'Receipt path must not be a symlink' if host_root.join(part).symlink?
      end
      receipt = JSON.parse(receipt_path.read)
      raise 'Stale host pubspec' unless receipt['pubspec_sha256'] == Digest::SHA256.file(host_root.join('pubspec.yaml')).hexdigest
      raise 'Receipt macOS mode disagrees with host' unless receipt['macos'] == requested_macos
      groups = %w[inbounds outbounds endpoints dns transports tls api services certificate_providers]
      fingerprint = receipt.fetch('fingerprint')
      raise 'Invalid receipt schema or identity' unless receipt['schema'] == 1 && fingerprint.is_a?(String) && fingerprint.match?(/\A[0-9a-f]{64}\z/) && receipt['version'].is_a?(String) && !receipt['version'].empty?
      features = receipt.fetch('features')
      raise 'Invalid receipt features' unless features.is_a?(Hash) && features.keys.sort == groups.sort && features.values.all? { |v| v.is_a?(Array) && v.all? { |x| x.is_a?(String) && !x.empty? } && v.uniq == v }
      artifact = receipt.fetch('artifacts').fetch(target)
      package_root = File.expand_path('..', pod_root)
      recipe_checker = File.join(package_root, 'tool/core_recipe.py')
      artifact_digest = nil
      recipe_python = [ENV['SINGBOX_PYTHON'], 'python3.14', 'python3.13', 'python3.12', 'python3.11', 'python3', 'python'].compact.uniq.find do |python|
        begin
          output, _error, status = Open3.capture3(python, '-I', recipe_checker, '--project-dir', host_root.to_s, '--target', target, '--print-artifact-digest')
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
      framework = resolve.call(artifact.fetch('path'))
      capabilities = resolve.call(artifact.fetch('capabilities'))
      recipe_metadata = resolve.call(artifact.fetch('metadata'))
      @recipe_input_files = [receipt_path.to_s, host_root.join('pubspec.yaml').to_s, recipe_metadata.to_s,
                             File.join(package_root, 'native-core.json'), File.join(package_root, 'native/mobile'),
                             File.join(package_root, 'native/wgc'), File.join(package_root, 'tool')]
      raise 'Missing Apple XCFramework' unless framework.directory? && framework.basename.to_s == 'Libbox.xcframework' && framework.join('Info.plist').file?
      raise 'Missing core-capabilities.json' unless capabilities.file? && capabilities.basename.to_s == 'core-capabilities.json'
      # The shared recipe checker already verified the complete framework tree,
      # including symlink confinement. Keep the returned digest for Xcode's guard.
      @capabilities_sha256 = Digest::SHA256.file(capabilities).hexdigest
      raise 'Modified capabilities' unless artifact['capabilities_sha256'] == @capabilities_sha256
      manifest = JSON.parse(capabilities.read)
      raise 'Receipt and capabilities disagree' unless manifest['schema'] == 1 && manifest['version'] == receipt['version'] && manifest['fingerprint'] == fingerprint && manifest['features'] == features
      # Host-local static slices cannot be globbed by CocoaPods. Link through
      # SDK-specific search paths and reject SDKs not present in this artifact.
      libraries = Xcodeproj::Plist.read_from_path(framework.join('Info.plist'))['AvailableLibraries']
      raise 'Invalid XCFramework library list' unless libraries.is_a?(Array)
      @slice_paths = {}
      sdks = platform == :macos ? { 'macosx' => nil } : { 'iphoneos' => nil, 'iphonesimulator' => 'simulator' }
      sdks.each do |sdk, variant|
        candidates = libraries.select { |lib| lib['SupportedPlatform'] == (platform == :macos ? 'macos' : 'ios') && lib['SupportedPlatformVariant'] == variant }
        next if candidates.empty?
        raise 'Ambiguous Apple slice' unless candidates.length == 1
        lib = candidates.first
        required_architectures = platform == :macos ? %w[arm64 x86_64] : ['arm64']
        raise 'Missing required architecture' unless lib['SupportedArchitectures'].is_a?(Array) && (required_architectures - lib['SupportedArchitectures']).empty?
        slice = framework.join(lib.fetch('LibraryIdentifier'), lib.fetch('LibraryPath')).cleanpath
        raise 'Invalid framework slice' unless slice.directory? && slice.realpath.to_s.start_with?(framework.to_s + '/') && slice.basename.to_s == 'Libbox.framework' && slice.join('Libbox').file?
        @slice_paths[sdk] = slice.dirname.to_s
      end
      raise 'No supported Apple slices' if @slice_paths.empty?
      @framework_path = framework.to_s
      @capabilities_path = capabilities.to_s
    rescue StandardError
      # Do not expose malformed receipt contents or parser diagnostics.
      raise "Invalid sing_box_flutter project #{platform == :macos ? 'macOS' : 'Apple'} artifacts. Rebuild the native core for this host before running pod install."
    end

    @profile_guard = <<-SCRIPT
set -eu
# Reject a rebuilt or changed profile until CocoaPods resolves its slices again.
#{Shellwords.escape(recipe_python)} -I #{Shellwords.escape(recipe_checker)} --project-dir #{Shellwords.escape(host_root.to_s)} --target #{target} --installed-artifact #{Shellwords.escape(artifact.fetch('path'))} --installed-sha256 #{Shellwords.escape(artifact_digest)}
current_fingerprint=$(/usr/bin/plutil -extract fingerprint raw -o - #{Shellwords.escape(receipt_path.to_s)})
current_pubspec_hash=$(/usr/bin/shasum -a 256 #{Shellwords.escape(host_root.join('pubspec.yaml').to_s)})
current_pubspec_hash=${current_pubspec_hash%% *}
if [ "$current_fingerprint" != #{Shellwords.escape(fingerprint)} ] || [ "$current_pubspec_hash" != #{Shellwords.escape(receipt['pubspec_sha256'])} ]; then
  printf 'Selected core profile changed. Rebuild the native core and run pod install before building this app.\\n' >&2
  exit 1
fi
    SCRIPT
  end

  # Shared is a dependency of every selected consumer, including the Go-free
  # controller. Unlike source settings, CocoaPods does not inherit script phases.
  # Keep one full guard here even when Xcode skips the incremental manifest copy.
  def validation_phase
    {
      :name => 'Validate selected core artifacts',
      :execution_position => :before_compile,
      :always_out_of_date => '1',
      :input_files => [capabilities_path, *recipe_input_files],
      :script => @profile_guard + <<-SCRIPT
source_manifest=#{Shellwords.escape(capabilities_path)}
actual_hash=$(/usr/bin/shasum -a 256 "$source_manifest")
actual_hash=${actual_hash%% *}
if [ "$actual_hash" != #{Shellwords.escape(capabilities_sha256)} ]; then
  printf 'Selected core capabilities changed; rebuild the native core and run pod install.\\n' >&2
  exit 1
fi
      SCRIPT
    }
  end

  def capabilities_install_phase
    resources = @platform == :macos ? 'sing_box_flutter_capabilities.bundle/Contents/Resources' : 'sing_box_flutter_capabilities.bundle'
    manifest = "#{resources}/core-capabilities.json"
    {
      :name => 'Install selected core capabilities',
      :execution_position => :before_compile,
      :input_files => [capabilities_path],
      :output_files => ["$(TARGET_BUILD_DIR)/#{manifest}"],
      :script => <<-SCRIPT
set -eu
/usr/bin/ditto #{Shellwords.escape(capabilities_path)} "${TARGET_BUILD_DIR}/#{manifest}"
      SCRIPT
    }
  end
end
