// swift-tools-version: 5.9
import PackageDescription

// The host's core build selects NE or user_tun in its own generated package.
let package = Package(
    name: "sing_box_flutter",
    platforms: [.macOS("12.0")],
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
            resources: [.process("PrivacyInfo.xcprivacy")]
        )
    ]
)
