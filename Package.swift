// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "vz-runner",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(
            name: "vz-runner",
            path: "Sources/vz-runner"
        ),
        .testTarget(
            name: "vz-runner-tests",
            dependencies: ["vz-runner"],
            path: "Tests/vz-runner-tests"
        )
    ]
)
