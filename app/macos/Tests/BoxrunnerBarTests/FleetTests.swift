import Foundation
import Testing
@testable import BoxrunnerBar

/// fleet.json is what the Worker's devices test gets from `GET /v1/fleet`.
@Test func decodesTheWorkersAnswer() throws {
	let url = try #require(Bundle.module.url(forResource: "fleet", withExtension: "json"))
	let fleet = try JSONDecoder().decode(Fleet.self, from: Data(contentsOf: url))
	#expect(fleet.boxes.map(\.name) == ["box-1"])
	#expect(fleet.running == 1)
	#expect(fleet.online == 1)
	#expect(fleet.device.by == "octo")
	#expect(fleet.boxes[0].paused == false)
	#expect(fleet.jobs.first?.memoryPeak == 9_812_000_768)
	#expect(fleet.jobs.first?.box == "box-1")
}

@Test func aBoxIsPausedOnlyWhenEveryClassIsLimitedToZero() {
	func box(_ limits: [Int?]) -> Fleet.Box {
		Fleet.Box(
			name: "mac", version: "0.9.3", online: true, secondsSinceSeen: 0, disk: .init(free: 1, floor: 1),
			classes: limits.enumerated().map { i, max in
				Fleet.Class(name: "c\(i)", max: 2, running: 0, starting: 0, paused: false, limit: max.map { .init(max: $0, by: "octo") })
			},
			runners: [])
	}
	#expect(box([0, 0]).paused)
	#expect(!box([0, nil]).paused)
	#expect(!box([0, 1]).paused)
	#expect(!box([]).paused)
}

@Test func writesDurationsAsTheDashboardDoes() {
	#expect(duration(45) == "45 s")
	#expect(duration(754) == "12 min")
	#expect(duration(3 * 3600 + 5 * 60) == "3 h 5 min")
	#expect(duration(7200) == "2 h")
	#expect(duration(2 * 86400) == "2 d")
	#expect(gigabytes(9_812_000_768) == "9.8 GB")
}
