// swift-tools-version: 6.0

import PackageDescription

let package = Package(
    name: "HomeSyncKit",
    platforms: [.macOS(.v14)],
    products: [
        // The whole engine, with no UI and no app bundle. Keeping it a plain
        // library is what lets it be tested from the terminal in seconds,
        // without Xcode and without code signing.
        .library(name: "HomeSyncKit", targets: ["HomeSyncKit"]),

        // A headless client. It exists so the engine can be driven by the
        // conformance suite before any interface exists, and so a sync problem
        // can be reproduced without launching the app.
        .executable(name: "homesync-cli", targets: ["homesync-cli"]),
    ],
    targets: [
        .target(
            name: "HomeSyncKit",
            swiftSettings: [.swiftLanguageMode(.v6)]
        ),
        .executableTarget(
            name: "homesync-cli",
            dependencies: ["HomeSyncKit"],
            swiftSettings: [.swiftLanguageMode(.v6)]
        ),
        .testTarget(
            name: "HomeSyncKitTests",
            dependencies: ["HomeSyncKit"],
            swiftSettings: [.swiftLanguageMode(.v6)]
        ),
    ]
)
