// SPDX-License-Identifier: GPL-3.0-or-later
import Foundation

// Run with: xcrun swift macos/Tests/check-built-bundles.swift /absolute/Host.app
// Foundation inspects the built app and its embedded extensions without loading
// executables, requesting VPN consent, or changing the host's network state.
struct BundleProbeFailure: Error, CustomStringConvertible {
    let description: String
}

func checkCapabilities(in ownerURL: URL) throws {
    guard let owner = Bundle(url: ownerURL),
          let bundleURL = owner.url(forResource: "sing_box_flutter_capabilities", withExtension: "bundle"),
          let bundle = Bundle(url: bundleURL) else {
        throw BundleProbeFailure(description: "Missing capabilities bundle in \(ownerURL.path)")
    }
    let resources = bundleURL.appendingPathComponent("Contents/Resources", isDirectory: true)
    guard bundle.resourceURL?.standardizedFileURL == resources.standardizedFileURL,
          let manifestURL = bundle.url(forResource: "core-capabilities", withExtension: "json"),
          manifestURL.standardizedFileURL == resources.appendingPathComponent("core-capabilities.json").standardizedFileURL else {
        throw BundleProbeFailure(description: "Foundation cannot resolve Contents/Resources/core-capabilities.json in \(bundleURL.path)")
    }
    let data = try Data(contentsOf: manifestURL)
    guard let manifest = try JSONSerialization.jsonObject(with: data) as? [String: Any],
          manifest["schema"] as? Int == 1,
          let version = manifest["version"] as? String, !version.isEmpty,
          let fingerprint = manifest["fingerprint"] as? String, !fingerprint.isEmpty,
          manifest["features"] is [String: [String]] else {
        throw BundleProbeFailure(description: "Unreadable core capabilities in \(manifestURL.path)")
    }
    print("Capabilities OK: \(ownerURL.path) (\(version), \(fingerprint))")
}

do {
    let paths = Array(CommandLine.arguments.dropFirst())
    guard !paths.isEmpty, paths.allSatisfy({ $0.hasPrefix("/") && $0.hasSuffix(".app") }) else {
        throw BundleProbeFailure(description: "Usage: xcrun swift check-built-bundles.swift /absolute/Host.app [...]")
    }
    for path in paths {
        let appURL = URL(fileURLWithPath: path, isDirectory: true)
        try checkCapabilities(in: appURL)
        let plugins = appURL.appendingPathComponent("Contents/PlugIns", isDirectory: true)
        if FileManager.default.fileExists(atPath: plugins.path) {
            let extensions = try FileManager.default.contentsOfDirectory(at: plugins, includingPropertiesForKeys: nil)
                .filter { $0.pathExtension == "appex" }.sorted { $0.path < $1.path }
            for extensionURL in extensions {
                try checkCapabilities(in: extensionURL)
            }
        }
    }
} catch {
    FileHandle.standardError.write(Data("Built-bundle check failed: \(error)\n".utf8))
    exit(1)
}
