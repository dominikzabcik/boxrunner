import AppKit
import Foundation
import SwiftUI
import Testing
@testable import BoxrunnerBar

/// Renders the menu in each phase from fleet.json into SNAPSHOT_DIR, when set,
/// to look at. The menu is dark in either appearance. AppKit draws it, so controls look as they do on screen.
@MainActor @Test func rendersTheMenu() throws {
	guard let dir = ProcessInfo.processInfo.environment["SNAPSHOT_DIR"] else { return }
	let url = try #require(Bundle.module.url(forResource: "fleet", withExtension: "json"))
	var fleet = try JSONDecoder().decode(Fleet.self, from: Data(contentsOf: url))
	var mac = fleet.boxes[0]
	mac.name = "mac-1"
	mac.classes = mac.classes.map { var c = $0; c.running = 0; c.starting = 0; c.limit = .init(max: 0, by: "octo"); return c }
	mac.runners = []
	fleet.boxes.append(mac)

	let paired = AppState(showing: fleet, fleetName: "SwarmNet", thisBox: "mac-1")
	let unpaired = AppState(showing: fleet, fleetName: "SwarmNet", thisBox: "")
	unpaired.phase = .unpaired
	unpaired.fleet = nil
	let pairing = AppState(showing: fleet, fleetName: "SwarmNet", thisBox: "")
	pairing.phase = .pairing(code: "K7QX-M4PT", url: URL(string: "https://box.test/pair?code=K7QX-M4PT")!)
	pairing.fleet = nil

	for (name, state) in [("paired", paired), ("unpaired", unpaired), ("pairing", pairing)] {
		for appearance in [NSAppearance.Name.darkAqua] {
			let host = NSHostingView(rootView: MenuView(state: state))
			host.appearance = NSAppearance(named: appearance)
			host.setFrameSize(host.fittingSize)
			host.layoutSubtreeIfNeeded()
			let rep = try #require(host.bitmapImageRepForCachingDisplay(in: host.bounds))
			host.cacheDisplay(in: host.bounds, to: rep)
			let png = try #require(rep.representation(using: .png, properties: [:]))
			try png.write(to: URL(filePath: dir).appending(path: "menu-\(name).png"))
		}
	}
}
