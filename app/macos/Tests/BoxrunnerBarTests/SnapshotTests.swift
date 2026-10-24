import AppKit
import Foundation
import SwiftUI
import Testing
@testable import BoxrunnerBar

/// Renders the menu from fleet.json into SNAPSHOT_DIR, when set, to look at.
@MainActor @Test func rendersTheMenu() throws {
	guard let dir = ProcessInfo.processInfo.environment["SNAPSHOT_DIR"] else { return }
	let url = try #require(Bundle.module.url(forResource: "fleet", withExtension: "json"))
	var fleet = try JSONDecoder().decode(Fleet.self, from: Data(contentsOf: url))
	var mac = fleet.boxes[0]
	mac.name = "mac-1"
	mac.classes = mac.classes.map { var c = $0; c.running = 0; c.limit = .init(max: 0, by: "octo"); return c }
	mac.runners = []
	fleet.boxes.append(mac)
	for scheme in [ColorScheme.light, .dark] {
		let view = MenuView(state: AppState(showing: fleet, fleetName: "Fleet", thisBox: "mac-1"))
			.background(Color(nsColor: .windowBackgroundColor))
			.environment(\.colorScheme, scheme)
		let renderer = ImageRenderer(content: view)
		renderer.scale = 2
		let image = try #require(renderer.nsImage)
		let tiff = try #require(image.tiffRepresentation)
		let rep = try #require(NSBitmapImageRep(data: tiff))
		let png = try #require(rep.representation(using: .png, properties: [:]))
		try png.write(to: URL(filePath: dir).appending(path: "menu-\(scheme).png"))
	}
}
