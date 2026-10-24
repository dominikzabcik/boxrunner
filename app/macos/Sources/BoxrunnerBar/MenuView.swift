import SwiftUI

/// The window under the menu bar item, drawn like a terminal UI: the fleet's
/// totals, this Mac's switch, every box, the waiting and the latest jobs.
struct MenuView: View {
	var state: AppState
	@Environment(\.openWindow) private var openWindow
	@Environment(\.openURL) private var openURL

	var body: some View {
		VStack(alignment: .leading, spacing: 16) {
			header
			switch (state.phase, state.fleet) {
			case (.paired, let fleet?):
				thisMac(fleet)
				boxes(fleet)
				if !fleet.waiting.isEmpty { waiting(fleet) }
				jobs(fleet)
			case (.paired, nil):
				Text("connecting…").font(Console.font()).foregroundStyle(Console.muted)
			default:
				PairView(state: state)
			}
			if let problem = state.problem {
				(Text("error ").foregroundColor(Console.red).bold() + Text(problem).foregroundColor(Console.text))
					.font(Console.font(11))
					.fixedSize(horizontal: false, vertical: true)
			}
			statusBar
		}
		.padding(.horizontal, 14).padding(.top, 14).padding(.bottom, 8)
		.frame(width: 380)
		.background(Console.background)
		.foregroundStyle(Console.text)
		.font(Console.font())
		.environment(\.colorScheme, .dark)
	}

	private var header: some View {
		VStack(alignment: .leading, spacing: 8) {
			HStack(alignment: .firstTextBaseline) {
				(Text(state.fleetName.lowercased()).foregroundColor(Console.text) + Text("_").foregroundColor(Console.accent))
					.font(Console.font(18, .bold))
				Spacer()
				live
			}
			if let fleet = state.fleet, state.phase == .paired {
				HStack(spacing: 14) {
					figure(fleet.running, " running", Console.blue)
					figure(fleet.waiting.count, " waiting", Console.yellow)
					figure(fleet.online, "/\(fleet.boxes.count) online", Console.green)
					Spacer()
				}
			}
		}
	}

	@ViewBuilder private var live: some View {
		switch state.phase {
		case .unpaired: Text("○ offline").foregroundStyle(Console.muted)
		case .pairing: Text("◌ pairing").foregroundStyle(Console.accent)
		case .paired:
			let live = state.problem == nil && state.fleet != nil
			Text(live ? "● live" : "● stale").foregroundStyle(live ? Console.green : Console.yellow)
		}
	}

	private func figure(_ value: Int, _ label: String, _ tint: Color) -> some View {
		(Text("\(value)").foregroundColor(value == 0 ? Console.muted : tint).bold() + Text(label).foregroundColor(Console.muted))
			.font(Console.font(12))
			.contentTransition(.numericText())
	}

	@ViewBuilder private func thisMac(_ fleet: Fleet) -> some View {
		if let box = state.myBox {
			Block(title: "this mac", trailing: box.name, accent: box.paused ? Console.yellow : Console.green) {
				HStack {
					VStack(alignment: .leading, spacing: 2) {
						Text(!box.online ? "offline" : box.paused ? "paused" : "taking jobs").bold()
						Text(!box.online ? "the box is not reporting" : box.paused ? "finishes what runs, takes nothing new" : "the fleet sends it jobs")
							.foregroundStyle(Console.muted).font(Console.font(11))
					}
					Spacer()
					ConsoleSwitch(on: !box.paused, disabled: state.busy || !box.online) { state.setAvailable(box.paused) }
				}
			}
		} else {
			Block(title: "this mac", accent: Console.accent) {
				HStack {
					Text("which box runs here?").foregroundStyle(Console.muted)
					Spacer()
					Menu {
						ForEach(fleet.boxes) { box in Button(box.name) { state.thisBox = box.name } }
					} label: {
						Text("choose ▾").foregroundStyle(Console.accent)
					}
					.menuStyle(.borderlessButton).fixedSize()
				}
			}
		}
	}

	private func boxes(_ fleet: Fleet) -> some View {
		Block(title: "boxes", trailing: "\(fleet.boxes.count)") {
			ForEach(fleet.boxes) { box in
				BoxRow(box: box, isThisMac: box.name == state.thisBox)
			}
		}
	}

	private func waiting(_ fleet: Fleet) -> some View {
		Block(title: "waiting", trailing: "\(fleet.waiting.count)", accent: Console.yellow) {
			ForEach(Array(fleet.waiting.enumerated()), id: \.offset) { _, w in
				VStack(alignment: .leading, spacing: 1) {
					HStack {
						(Text("◷ ").foregroundColor(Console.yellow) + Text(repoName(w.repo)) + Text(" \(w.class)").foregroundColor(Console.muted))
						Spacer()
						Text(duration(w.seconds)).foregroundStyle(Console.muted)
					}
					if let cause = w.cause {
						Text("  \(cause)").font(Console.font(10)).foregroundStyle(Console.muted).fixedSize(horizontal: false, vertical: true)
					}
				}
			}
		}
	}

	private func jobs(_ fleet: Fleet) -> some View {
		Block(title: "recent jobs") {
			if fleet.jobs.isEmpty {
				Text("no job has finished yet").foregroundStyle(Console.muted)
			}
			ForEach(fleet.jobs.prefix(5)) { job in
				HStack(spacing: 8) {
					Text(job.result == "succeeded" ? "✓" : job.result == "cancelled" ? "–" : "✗")
						.foregroundStyle(job.result == "succeeded" ? Console.green : job.result == "cancelled" ? Console.muted : Console.red)
						.bold()
					VStack(alignment: .leading, spacing: 0) {
						Text(job.name).lineLimit(1)
						Text([repoName(job.repo), job.box, job.memoryPeak.map(gigabytes)].compactMap { $0 }.joined(separator: " · "))
							.font(Console.font(10)).foregroundStyle(Console.muted).lineLimit(1)
					}
					Spacer(minLength: 6)
					VStack(alignment: .trailing, spacing: 0) {
						Text(duration(job.durationSeconds))
						Text("\(duration(job.secondsSinceFinished)) ago").font(Console.font(10)).foregroundStyle(Console.faint)
					}
				}
			}
		}
	}

	/// The bottom line, as opencode keeps its keys: what each does and its shortcut.
	private var statusBar: some View {
		HStack(spacing: 2) {
			if let worker = state.worker, state.phase == .paired {
				KeyButton(title: "dashboard", key: "⌘D") { openURL(worker) }
					.keyboardShortcut("d")
			}
			KeyButton(title: "settings", key: "⌘,") { openWindow(id: "settings"); NSApp.activate() }
				.keyboardShortcut(",")
			KeyButton(title: "quit", key: "⌘Q") { NSApp.terminate(nil) }
				.keyboardShortcut("q")
			Spacer()
			if let updated = state.updated, state.phase == .paired {
				Text(updated, style: .relative).font(Console.font(10)).foregroundStyle(Console.faint)
			}
		}
		.padding(.top, 6)
		.overlay(alignment: .top) { Rectangle().fill(Console.line).frame(height: 1) }
	}
}

/// One box: its health, the cells of each class and what runs there.
struct BoxRow: View {
	var box: Fleet.Box
	var isThisMac: Bool

	var body: some View {
		VStack(alignment: .leading, spacing: 3) {
			HStack(spacing: 6) {
				StatusDot(health: Health(box))
				Text(box.name).bold()
				if isThisMac { Text("(you)").foregroundStyle(Console.accent) }
				Spacer()
				Text(status).foregroundStyle(Console.muted).font(Console.font(11))
			}
			if box.online {
				HStack(spacing: 12) {
					ForEach(box.classes) { c in
						(Text(c.name).foregroundColor(Console.muted) + Text(" ") + slots(c))
							.font(Console.font(11))
					}
				}
				.padding(.leading, 16)
				ForEach(box.runners.prefix(3)) { r in
					HStack {
						(Text("└ ").foregroundColor(Console.faint)
							+ Text(repoName(r.repo)).foregroundColor(r.state == "running" ? Console.text : Console.muted)
							+ Text(" \(r.state)").foregroundColor(r.state == "running" ? Console.blue : Console.muted))
						Spacer()
						Text(duration(r.seconds)).foregroundStyle(Console.muted)
					}
					.font(Console.font(11))
					.padding(.leading, 16)
				}
			}
		}
		.padding(.vertical, 2)
	}

	private var status: String {
		guard box.online else { return "offline \(duration(box.secondsSinceSeen))" }
		if box.paused { return "paused" }
		return "\(gigabytes(box.disk.free)) free  v\(box.version)"
	}
}

func repoName(_ repo: String) -> String {
	String(repo.split(separator: "/").last ?? Substring(repo))
}
