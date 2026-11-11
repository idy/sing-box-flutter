// SPDX-License-Identifier: GPL-3.0-or-later
import PackagePlugin

@main
struct SingBoxFlutterValidate: BuildToolPlugin {
    func createBuildCommands(context: PluginContext, target: Target) throws -> [Command] {
        // A prebuild command deliberately runs even when no Swift inputs changed.
        // Its only output is public capability metadata in the plugin work directory.
        [.prebuildCommand(
            displayName: "Validate selected sing-box core",
            executable: Path(@@PYTHON@@),
            arguments: ["-I", @@VALIDATOR@@, "validate", "--settings",
                        context.package.directory.appending("selection.json").string,
                        "--output", context.pluginWorkDirectory.string],
            outputFilesDirectory: context.pluginWorkDirectory
        )]
    }
}
