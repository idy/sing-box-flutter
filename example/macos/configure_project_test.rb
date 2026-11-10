require 'fileutils'
require 'minitest/autorun'
require 'tmpdir'
require_relative 'configure_project'

class MacOSExampleProjectTest < Minitest::Test
  def setup
    @temporary = Dir.mktmpdir('sing-box-macos-project-')
    @directory = File.join(@temporary, 'macos')
    FileUtils.mkdir_p(@directory)
    %w[Runner.xcodeproj Runner PacketTunnel].each do |name|
      FileUtils.cp_r(File.join(__dir__, name), @directory)
    end
  end

  def teardown
    FileUtils.remove_entry(@temporary)
  end

  def select_mode(mode)
    pubspec = mode ? { 'sing_box_flutter' => { 'macos' => { 'mode' => mode } } } : {}
    File.write(File.join(@temporary, 'pubspec.yaml'), YAML.dump(pubspec))
  end

  def configure
    capture_io { SingBoxFlutterMacOSExample.configure(@directory) }
    Xcodeproj::Project.open(File.join(@directory, 'Runner.xcodeproj'))
  end

  def assert_mode(project, mode)
    ne_mode = mode == 'ne'
    runner = project.targets.find { |target| target.name == 'Runner' }
    extension = project.targets.find { |target| target.name == 'PacketTunnel' }
    assert_equal 1, project.targets.count { |target| target.name == 'PacketTunnel' }
    project.build_configurations.each do |configuration|
      assert_equal 'Warnings.xcconfig', configuration.base_configuration_reference.path
    end
    runner.build_configurations.each do |configuration|
      expected = configuration.name == 'Debug' ? 'Debug.xcconfig' : 'Release.xcconfig'
      assert_equal expected, configuration.base_configuration_reference.path
      assert_includes File.read(configuration.base_configuration_reference.real_path), 'AppInfo.xcconfig'
    end
    assemble = project.targets.find { |target| target.name == 'Flutter Assemble' }
    assemble.build_configurations.each do |configuration|
      assert_equal 'ephemeral/Flutter-Generated.xcconfig', configuration.base_configuration_reference.path
    end
    extension.build_configurations.each do |configuration|
      refute_match(/Pods-Runner|Runner\/Configs|Flutter-/, configuration.base_configuration_reference&.path.to_s)
    end
    assert_equal 1, extension.source_build_phase.files.count { |file| file.file_ref.path == 'PacketTunnelProvider.swift' }
    assert_equal(ne_mode ? 1 : 0, runner.dependencies.count { |dependency| dependency.target == extension })
    embeds = runner.copy_files_build_phases.select { |phase| phase.name == 'Embed App Extensions' }
    assert_equal(ne_mode ? 1 : 0, embeds.length)
    if ne_mode
      assert_equal [extension.product_reference], embeds.first.files_references
      assert_equal '13', embeds.first.dst_subfolder_spec
    end
    runner.build_configurations.each do |configuration|
      settings = configuration.build_settings
      assert_equal(ne_mode ? 'YES' : 'NO', settings['ENABLE_APP_SANDBOX'])
      entitlements = Xcodeproj::Plist.read_from_path(File.join(@directory, settings.fetch('CODE_SIGN_ENTITLEMENTS')))
      assert_equal ne_mode, entitlements['com.apple.security.app-sandbox']
      if ne_mode
        assert_equal ['group.dev.idy.singboxflutter.example'], entitlements['com.apple.security.application-groups']
        assert_equal ['packet-tunnel-provider'], entitlements['com.apple.developer.networking.networkextension']
        assert entitlements['com.apple.security.network.client']
      else
        refute entitlements.key?('com.apple.security.application-groups')
        refute entitlements.key?('com.apple.developer.networking.networkextension')
      end
    end
    [project, *project.targets].each do |target|
      target.build_configurations.each do |configuration|
        assert_equal '12.0', configuration.build_settings['MACOSX_DEPLOYMENT_TARGET']
        refute_equal '-', configuration.build_settings['CODE_SIGN_IDENTITY'] if ne_mode
      end
    end
    extension.build_configurations.each do |configuration|
      assert_equal 'dev.idy.singBoxFlutterExample.PacketTunnel', configuration.build_settings['PRODUCT_BUNDLE_IDENTIFIER']
      assert_equal 'YES', configuration.build_settings['APPLICATION_EXTENSION_API_ONLY']
    end
    refute project.objects.any? { |object| object.display_name.include?('FlutterGeneratedPluginSwiftPackage') }
  end

  def test_default_ne_and_reversible_mode_changes_are_idempotent
    [nil, 'user_tun', 'ne', 'user_tun'].each do |mode|
      select_mode(mode)
      project = configure
      assert_mode(project, mode || 'ne')
      first = File.binread(project.path.join('project.pbxproj'))
      configure
      assert_equal first, File.binread(project.path.join('project.pbxproj'))
    end
  end

  def test_preserves_developer_signing_and_release_settings
    select_mode('ne')
    project = configure
    project.targets.each do |target|
      target.build_configurations.each do |configuration|
        configuration.build_settings['DEVELOPMENT_TEAM'] = 'LOCALTEAM'
        configuration.build_settings['CODE_SIGN_IDENTITY'] = 'Developer Identity'
        if target.name == 'PacketTunnel'
          configuration.build_settings['PRODUCT_BUNDLE_IDENTIFIER'] = 'custom.example.PacketTunnel'
          configuration.build_settings['MARKETING_VERSION'] = '2.3.4'
          configuration.build_settings['CURRENT_PROJECT_VERSION'] = '42'
          configuration.build_settings['PRODUCT_MODULE_NAME'] = 'CustomPacketTunnel'
          configuration.build_settings['LD_RUNPATH_SEARCH_PATHS'] = ['$(inherited)', '@loader_path/CustomFrameworks']
        end
      end
    end
    project.save
    ['user_tun', 'ne'].each do |mode|
      select_mode(mode)
      configure.targets.each do |target|
        target.build_configurations.each do |configuration|
          assert_equal 'LOCALTEAM', configuration.build_settings['DEVELOPMENT_TEAM']
          assert_equal 'Developer Identity', configuration.build_settings['CODE_SIGN_IDENTITY']
          if target.name == 'PacketTunnel'
            assert_equal 'custom.example.PacketTunnel', configuration.build_settings['PRODUCT_BUNDLE_IDENTIFIER']
            assert_equal '2.3.4', configuration.build_settings['MARKETING_VERSION']
            assert_equal '42', configuration.build_settings['CURRENT_PROJECT_VERSION']
            assert_equal 'CustomPacketTunnel', configuration.build_settings['PRODUCT_MODULE_NAME']
            assert_equal ['$(inherited)', '@loader_path/CustomFrameworks'], configuration.build_settings['LD_RUNPATH_SEARCH_PATHS']
          end
        end
      end
    end
  end

  def test_ne_removes_only_ad_hoc_signing_defaults
    select_mode('user_tun')
    project = configure
    [project, *project.targets].each do |target|
      target.build_configurations.each do |configuration|
        configuration.build_settings['CODE_SIGN_IDENTITY'] = '-'
      end
    end
    project.save
    # Direct TUN hosts may still choose ad-hoc signing deliberately.
    configure.build_configurations.each do |configuration|
      assert_equal '-', configuration.build_settings['CODE_SIGN_IDENTITY']
    end
    select_mode('ne')
    project = configure
    [project, *project.targets].each do |target|
      target.build_configurations.each do |configuration|
        refute_equal '-', configuration.build_settings['CODE_SIGN_IDENTITY']
      end
    end
  end

  def test_empty_macos_mapping_defaults_to_ne
    File.write(File.join(@temporary, 'pubspec.yaml'), YAML.dump({ 'sing_box_flutter' => { 'macos' => {} } }))
    assert_equal 'ne', SingBoxFlutterMacOSExample.mode(@directory)
    assert_mode(configure, 'ne')
  end

  def test_rejects_unknown_mode_before_changing_project
    select_mode('unsupported')
    path = File.join(@directory, 'Runner.xcodeproj/project.pbxproj')
    original = File.binread(path)
    error = assert_raises(RuntimeError) { configure }
    assert_match(/mode: ne or user_tun/, error.message)
    assert_equal original, File.binread(path)
  end
end
