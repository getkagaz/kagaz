// swift-tools-version: 6.0
//
// kagaz-machelper — the macOS leaf utility used by the Kagaz Go core for
// on-device OCR (Vision) and on-device classification (Apple Foundation
// Models). It has NO external package dependencies on purpose: the Homebrew
// formula must be able to build it offline from a source tarball.

import PackageDescription

let package = Package(
    name: "kagaz-machelper",
    platforms: [
        .macOS(.v15)
    ],
    products: [
        .executable(name: "kagaz-machelper", targets: ["MacHelper"])
    ],
    dependencies: [
        // Intentionally empty. Do not add swift-argument-parser or anything
        // else here; argument parsing is hand-rolled in Arguments.swift.
    ],
    targets: [
        .executableTarget(
            name: "MacHelper",
            path: "Sources/MacHelper",
            swiftSettings: [
                .swiftLanguageMode(.v5)
            ]
        )
    ]
)
