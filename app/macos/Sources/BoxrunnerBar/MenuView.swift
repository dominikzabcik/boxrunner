import SwiftUI

/// The window under the menu bar item: this Mac's availability, every box,
/// the waiting jobs and the latest ones that finished.
struct MenuView: View {
	var state: AppState
	@Environment(\.openWindow) private var openWindow
	@Environment(\.openURL) private var openURL

	var body: some View {
		VStack(alignment: .leading, spacing: 12) {
			header
			if state.phase == .paired, let fleet = state.fleet {
				thisMac(fleet)
				Divider()
				boxes(fleet)
				if !fleet.waiting.isEmpty { waiting(fleet) }
				jobs(fleet)
			} else if state.phase == .paired {
				ProgressView().frame(maxWidth: .infinity)
			} else {
				unpaired
			}
			if let problem = state.problem {
				Label(problem, systemImage: "exclamationmark.triangle").foregroundStyle(.orange).font(.callout)
			}
			Divider()
			footer
		}
		.padding(14)
		.frame(width: 360)
	}

	private var header: some View {
		HStack(alignment: .firstTextBaseline) {
			VStack(alignment: .leading, spacing: 2) {
				Text(state.fleetName).font(.headline)
				if let fleet = state.fleet {
					Text("\(fleet.online) of \(fleet.boxes.count) boxes online · \(fleet.running) running · \(fleet.waiting.count) waiting")
						.font(.caption).foregroundStyle(.secondary)
				}
			}
			Spacer()
			if let fleet = state.fleet { Text(fleet.device.name).font(.caption).foregroundStyle(.tertiary) }
		}
	}

	@ViewBuilder private func thisMac(_ fleet: Fleet) -> some View {
		if let box = state.myBox {
			HStack {
				VStack(alignment: .leading, spacing: 2) {
					Text("This Mac").font(.subheadline.weight(.semibold))
					Text(box.paused ? "\(box.name) takes no new jobs" : "\(box.name) takes jobs")
						.font(.caption).foregroundStyle(.secondary)
				}
				Spacer()
				Toggle("Available", isOn: Binding(get: { !box.paused }, set: { state.setAvailable($0) }))
					.toggleStyle(.switch).labelsHidden().disabled(state.busy || !box.online)
			}
		} else {
			HStack {
				Text("Which box is this Mac?").font(.subheadline)
				Spacer()
				Picker("", selection: Binding(get: { state.thisBox }, set: { state.thisBox = $0 })) {
					Text("None").tag("")
					ForEach(fleet.boxes) { Text($0.name).tag($0.name) }
				}
				.labelsHidden().fixedSize()
			}
		}
	}

	private func boxes(_ fleet: Fleet) -> some View {
		VStack(alignment: .leading, spacing: 8) {
			Text("Boxes").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
			ForEach(fleet.boxes) { box in
				HStack(alignment: .top, spacing: 8) {
					Circle().fill(color(box)).frame(width: 8, height: 8).padding(.top, 5)
					VStack(alignment: .leading, spacing: 2) {
						HStack {
							Text(box.name).font(.body.weight(.medium))
							Spacer()
							Text(box.online ? "\(box.running) running" + (box.starting > 0 ? ", \(box.starting) starting" : "") : "offline \(duration(box.secondsSinceSeen))")
								.font(.caption).foregroundStyle(.secondary).monospacedDigit()
						}
						Text(box.classes.map { c in
							"\(c.name) \(c.running)/\(c.limit.map { min($0.max, c.max) } ?? c.max)" + (c.paused ? " paused" : "")
						}.joined(separator: " · ") + " · \(gigabytes(box.disk.free)) free · \(box.version)")
							.font(.caption).foregroundStyle(.secondary)
						ForEach(box.runners) { r in
							Text("\(r.state) \(r.repo) · \(duration(r.seconds))").font(.caption2).foregroundStyle(.tertiary)
						}
					}
				}
			}
		}
	}

	private func waiting(_ fleet: Fleet) -> some View {
		VStack(alignment: .leading, spacing: 4) {
			Text("Waiting").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
			ForEach(Array(fleet.waiting.enumerated()), id: \.offset) { _, w in
				Text("\(w.repo) · \(w.class) · \(duration(w.seconds))" + (w.cause.map { " · \($0)" } ?? ""))
					.font(.caption).foregroundStyle(.orange)
			}
		}
	}

	private func jobs(_ fleet: Fleet) -> some View {
		VStack(alignment: .leading, spacing: 4) {
			Text("Recent jobs").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
			if fleet.jobs.isEmpty { Text("None yet").font(.caption).foregroundStyle(.tertiary) }
			ForEach(fleet.jobs.prefix(6)) { job in
				HStack(spacing: 6) {
					Image(systemName: job.result == "succeeded" ? "checkmark.circle.fill" : job.result == "cancelled" ? "minus.circle.fill" : "xmark.circle.fill")
						.foregroundStyle(job.result == "succeeded" ? .green : job.result == "cancelled" ? .secondary : .red)
					VStack(alignment: .leading, spacing: 0) {
						Text("\(job.repo.split(separator: "/").last ?? "") · \(job.name)").font(.caption).lineLimit(1)
						Text([job.box, duration(job.durationSeconds), job.memoryPeak.map(gigabytes)].compactMap { $0 }.joined(separator: " · ")
							+ " · \(duration(job.secondsSinceFinished)) ago")
							.font(.caption2).foregroundStyle(.tertiary)
					}
				}
			}
		}
	}

	private var unpaired: some View {
		VStack(alignment: .leading, spacing: 8) {
			if case .pairing(let code, _) = state.phase {
				Text("Approve the pairing in your browser. The code there:").font(.callout)
				Text(code).font(.system(.title, design: .monospaced).weight(.semibold))
			} else {
				Text("Pair this Mac with your boxrunner Worker to see the fleet and switch your box on and off.").font(.callout)
			}
			Button("Pair…") { openWindow(id: "settings"); NSApp.activate() }
		}
	}

	private var footer: some View {
		HStack {
			if let updated = state.updated {
				Text("Updated \(updated, style: .relative) ago").font(.caption2).foregroundStyle(.tertiary)
			}
			Spacer()
			if let worker = state.worker, state.phase == .paired {
				Button("Dashboard") { openURL(worker) }
			}
			Button("Settings…") { openWindow(id: "settings"); NSApp.activate() }
			Button("Quit") { NSApp.terminate(nil) }
		}
		.buttonStyle(.borderless).font(.caption)
	}

	private func color(_ box: Fleet.Box) -> Color {
		if !box.online { return .gray }
		if box.paused { return .orange }
		return box.running > 0 ? .blue : .green
	}
}
