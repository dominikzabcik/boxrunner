// swift-tools-version: 6.0
import PackageDescription

let package = Package(
	name: "BoxrunnerBar",
	platforms: [.macOS(.v14)],
	targets: [
		.executableTarget(name: "BoxrunnerBar", path: "Sources/BoxrunnerBar"),
		.testTarget(name: "BoxrunnerBarTests", dependencies: ["BoxrunnerBar"], path: "Tests/BoxrunnerBarTests", resources: [.copy("fleet.json")]),
	]
)
