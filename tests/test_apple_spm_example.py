"""Fixture editing and real Xcode project wiring without Flutter/Go builds."""
import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('check_apple_spm', ROOT / 'tool/check_apple_spm.py')
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class AppleSPMExample(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        shutil.copytree(ROOT / 'example', self.root / 'example',
                        ignore=lambda directory, names: [name for name in names if name in smoke.EXCLUDED])

    def tearDown(self):
        self.temporary.cleanup()

    def test_isolated_hosts_preserve_features_and_exclude_generated_files(self):
        for name in ('Pods', 'ephemeral', '.dart_tool', 'build'):
            path = self.root / 'example/macos' / name
            path.mkdir(exist_ok=True)
            (path / 'stale').touch()
        original = (self.root / 'example/pubspec.yaml').read_text()
        first = smoke.create_fixture(self.root, 'macos', 'ne')
        second = smoke.create_fixture(self.root, 'macos', 'user_tun')
        self.assertNotEqual(first, second)
        self.assertEqual(original, (self.root / 'example/pubspec.yaml').read_text())
        for host, mode in ((first, 'ne'), (second, 'user_tun')):
            text = (host / 'pubspec.yaml').read_text()
            self.assertIn('enable-swift-package-manager: true', text)
            self.assertIn('mode: ' + mode, text)
            self.assertIn(str(self.root), text)
            self.assertEqual(original.split('  core:')[1].split('\nflutter:')[0],
                             text.split('  core:')[1].split('\nflutter:')[0])
            for name in ('Pods', 'ephemeral', '.dart_tool', 'build'):
                self.assertFalse((host / 'macos' / name).exists())

    def test_rejects_ios_user_tun_and_unrecognized_pubspec(self):
        with self.assertRaises(ValueError):
            smoke.create_fixture(self.root, 'ios', 'user_tun')
        with self.assertRaises(ValueError):
            smoke.fixture_pubspec('unexpected: yaml\n', self.root, 'ne')

    def test_native_wiring_is_reversible_and_preserves_flutter_and_signing(self):
        ruby = os.environ.get('RUBY', 'ruby')
        available = subprocess.run([ruby, '-e', 'require "xcodeproj"'], capture_output=True)
        if available.returncode:
            self.skipTest('Set RUBY/GEM_HOME to a Ruby with xcodeproj for native project tests')
        script = r'''
require ARGV.shift
platform, directory, aggregate_state = ARGV
path = File.join(directory, 'Runner.xcodeproj')
pubspec_path = File.join(directory, '..', 'pubspec.yaml')
project = Xcodeproj::Project.open(path)
runner = project.targets.find { |target| target.name == 'Runner' }
extension = project.targets.find { |target| target.name == 'PacketTunnel' }
extension.build_configurations.each { |c| c.build_settings['DEVELOPMENT_TEAM'] = 'LOCALTEAM' }
aggregate = project.new(Xcodeproj::Project::Object::XCLocalSwiftPackageReference)
aggregate.relative_path = 'Flutter/ephemeral/Packages/FlutterGeneratedPluginSwiftPackage'
project.root_object.package_references << aggregate
fixed_id = '78A318202AECB46A00862997'
fixed_file = project.objects_by_uuid.fetch(fixed_id)
raise 'fixture no longer reproduces orphan' unless fixed_file.product_ref.nil? && fixed_file.file_ref.nil?
if aggregate_state == 'valid'
  product = project.new(Xcodeproj::Project::Object::XCSwiftPackageProductDependency)
  product.product_name = 'FlutterGeneratedPluginSwiftPackage'
  product.package = aggregate
  runner.package_product_dependencies << product
  fixed_file.product_ref = product
end
project.save
configure = lambda do
  if platform == 'macos'
    SingBoxFlutterMacOSExample.configure(directory)
  else
    SingBoxFlutterExampleSPM.configure(directory)
  end
end
[true, true, false, true].each do |enabled|
  pubspec = YAML.safe_load(File.read(pubspec_path))
  pubspec['flutter']['config']['enable-swift-package-manager'] = enabled
  File.write(pubspec_path, YAML.dump(pubspec))
  configure.call
  first = File.binread(File.join(path, 'project.pbxproj'))
  configure.call
  raise 'not idempotent' unless first == File.binread(File.join(path, 'project.pbxproj'))
  project = Xcodeproj::Project.open(path)
  extension = project.targets.find { |target| target.name == 'PacketTunnel' }
  products = extension.package_product_dependencies.select { |p| p.product_name == SingBoxFlutterExampleSPM::PRODUCT }
  raise 'incorrect products' unless products.length == (enabled ? 1 : 0)
  extension.build_configurations.each do |c|
    raise 'signing changed' unless c.build_settings['DEVELOPMENT_TEAM'] == 'LOCALTEAM'
    raise 'extension still inherits Pods' if enabled && c.base_configuration_reference&.path.to_s.include?('Pods-PacketTunnel')
  end
  if enabled
    raise 'missing framework link' unless extension.frameworks_build_phase.files.count { |f| f.product_ref == products.first } == 1
    raise 'stale CocoaPods phase' if extension.shell_script_build_phases.any? { |p| p.name.to_s.start_with?('[CP]') }
  end
  # Before choosing CocoaPods, the Flutter-generated aggregate must survive.
  if enabled && aggregate
    raise 'Flutter aggregate removed' unless project.root_object.package_references.any? { |p| p.uuid == aggregate.uuid }
    fixed_file = project.objects_by_uuid[fixed_id]
    if aggregate_state == 'valid'
      raise 'valid Flutter build file removed' unless fixed_file&.product_ref&.product_name == 'FlutterGeneratedPluginSwiftPackage'
    else
      raise 'orphan Flutter migration UUID survives' if fixed_file
    end
  end
  aggregate = nil unless enabled
end
if platform == 'macos'
  pubspec = YAML.safe_load(File.read(pubspec_path))
  pubspec['sing_box_flutter']['macos']['mode'] = 'user_tun'
  File.write(pubspec_path, YAML.dump(pubspec))
  configure.call
  project = Xcodeproj::Project.open(path)
  runner = project.targets.find { |t| t.name == 'Runner' }
  extension = project.targets.find { |t| t.name == 'PacketTunnel' }
  raise 'user_tun still links extension' unless extension.package_product_dependencies.empty?
  raise 'user_tun still builds extension' if runner.dependencies.any? { |d| d.target == extension }
  raise 'user_tun still embeds extension' if runner.copy_files_build_phases.any? { |p| p.name == 'Embed App Extensions' }
end
'''
        for platform in ('ios', 'macos'):
            for state in ('orphan', 'valid'):
                with self.subTest(platform=platform, aggregate=state):
                    host = smoke.create_fixture(self.root, platform, 'ne')
                    configure = host / platform / ('configure_spm.rb' if platform == 'ios' else 'configure_project.rb')
                    subprocess.run([ruby, '-e', script, str(configure), platform, str(host / platform), state],
                                   check=True, capture_output=True, text=True)


if __name__ == '__main__':
    unittest.main()
