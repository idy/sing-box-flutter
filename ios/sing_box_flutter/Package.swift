// swift-tools-version: 5.9
import PackageDescription

// Flutter resolves this package through its host-local .packages symlink.
// The explicit core build prepares the adjacent host-owned selection package.
let package = Package(
    name: "sing_box_flutter",
    platforms: [.iOS("15.0")],
    products: [.library(name: "sing-box-flutter", targets: ["sing_box_flutter"])],
    dependencies: [
        .package(name: "FlutterFramework", path: "../FlutterFramework"),
        .package(name: "SingBoxFlutterHost", path: "../../SingBoxFlutterHost")
    ],
    targets: [
        .target(
            name: "sing_box_flutter",
            dependencies: [
                .product(name: "FlutterFramework", package: "FlutterFramework"),
                .product(name: "SingBoxFlutterHost", package: "SingBoxFlutterHost")
            ],
            exclude: ["SingBoxFlutterPlugin.swift"],
            sources: ["SPMExports.swift"],
            resources: [.process("PrivacyInfo.xcprivacy")]
        )
    ]
)
