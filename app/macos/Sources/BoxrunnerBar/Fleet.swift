import Foundation

/// What `GET /v1/fleet` answers: the dashboard's data, the parts the app shows.
struct Fleet: Decodable, Equatable, Sendable {
	var boxes: [Box]
	var waiting: [Waiting]
	var jobs: [Job]
	var device: Device

	struct Box: Decodable, Equatable, Identifiable, Sendable {
		var name: String
		var version: String
		var online: Bool
		var secondsSinceSeen: Int
		var disk: Disk
		var classes: [Class]
		var runners: [Runner]
		var id: String { name }

		/// The box takes no new jobs: every class it serves has a limit of 0.
		var paused: Bool { !classes.isEmpty && classes.allSatisfy { $0.limit?.max == 0 } }
		var running: Int { classes.reduce(0) { $0 + $1.running } }
		var starting: Int { classes.reduce(0) { $0 + $1.starting } }
	}

	struct Disk: Decodable, Equatable, Sendable {
		var free: Int64
		var floor: Int64
	}

	struct Class: Decodable, Equatable, Identifiable, Sendable {
		var name: String
		var max: Int
		var running: Int
		var starting: Int
		var paused: Bool
		var limit: Limit?
		var id: String { name }
	}

	struct Limit: Decodable, Equatable, Sendable {
		var max: Int
		var by: String
	}

	struct Runner: Decodable, Equatable, Identifiable, Sendable {
		var name: String
		var repo: String
		var `class`: String
		var state: String
		var seconds: Int
		var id: String { name }
	}

	struct Waiting: Decodable, Equatable, Sendable {
		var repo: String
		var `class`: String
		var box: String
		var seconds: Int
		var cause: String?
	}

	struct Job: Decodable, Equatable, Identifiable, Sendable {
		var runner: String
		var repo: String
		var `class`: String
		var name: String
		var result: String
		var finished: String
		var secondsSinceFinished: Int
		var durationSeconds: Int
		var box: String?
		var cpuSeconds: Double?
		var memoryPeak: Int64?
		var id: String { runner + finished }
	}

	struct Device: Decodable, Equatable, Sendable {
		var id: String
		var name: String
		var by: String
	}

	var running: Int { boxes.reduce(0) { $0 + $1.running } }
	var starting: Int { boxes.reduce(0) { $0 + $1.starting } }
	var online: Int { boxes.filter(\.online).count }
}

/// "45 s", "12 min", "3 h 5 min", "2 d".
func duration(_ seconds: Int) -> String {
	switch seconds {
	case ..<60: "\(seconds) s"
	case ..<3600: "\(seconds / 60) min"
	case ..<86400: seconds % 3600 >= 60 ? "\(seconds / 3600) h \(seconds % 3600 / 60) min" : "\(seconds / 3600) h"
	default: "\(seconds / 86400) d"
	}
}

/// "9.8 GB", one decimal, as the dashboard writes it.
func gigabytes(_ bytes: Int64) -> String {
	String(format: "%.1f GB", Double(bytes) / 1_000_000_000)
}
