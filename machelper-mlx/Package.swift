// swift-tools-version: 6.0
//
// kagaz-machelper-mlx — the opt-in MLX classification helper for Kagaz.
//
// This is a SEPARATE package from machelper/ on purpose. The default Homebrew
// formula builds kagaz-machelper, which has zero package dependencies; this
// one pulls MLX and a few hundred megabytes of Swift package graph, so it ships
// as its own opt-in formula and is never on the default install path.

import PackageDescription

let package = Package(
    name: "kagaz-machelper-mlx",
    platforms: [
        .macOS(.v15)
    ],
    products: [
        .executable(name: "kagaz-machelper-mlx", targets: ["MacHelperMLX"])
    ],
    dependencies: [
        .package(url: "https://github.com/ml-explore/mlx-swift-examples.git", from: "2.25.9")
    ],
    targets: [
        .executableTarget(
            name: "MacHelperMLX",
            dependencies: [
                .product(name: "MLXLLM", package: "mlx-swift-examples"),
                .product(name: "MLXLMCommon", package: "mlx-swift-examples"),
            ],
            path: "Sources/MacHelperMLX",
            swiftSettings: [
                .swiftLanguageMode(.v5)
            ]
        )
    ]
)
