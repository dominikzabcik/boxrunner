import SwiftUI

/// The window under the menu bar item: the fleet's totals, this Mac's
/// availability, every box, the waiting jobs and the latest ones that finished.
struct MenuView: View {
	var state: AppState
	@Environment(\.openWindow) private var openWindow
	@Environment(\.openURL) private var openURL

	var body: some View {
		VStack(alignment: .leading, spacing: 14) {
			header
			switch (state.phase, state.fleet) {
			case (.paired, let fleet?):
				totals(fleet)
				thisMac(fleet)
				boxes(fleet)
				if !fleet.waiting.isEmpty { waiting(fleet) }
				jobs(fleet)
			case (.paired, nil):
				ProgressView().controlSize(.small).frame(maxWidth: .infinity, minHeight: 80)
			default:
				PairView(state: state)
			}
			if let problem = state.problem {
				Label(problem, systemImage: "exclamationmark.triangle.fill")
					.font(.caption).foregroundStyle(.orange)
					.fixedSize(horizontal: false, vertical: true)
			}
			footer
		}
		.padding(14)
		.frame(width: 340)
		.animation(.snappy, value: state.fleet)
	}

	private var header: some View {
		HStack(spacing: 10) {
			Mark()
			VStack(alignment: .leading, spacing: 1) {
				Text(state.fleetName).font(.system(size: 15, weight: .semibold))
				Text(subtitle).font(.caption).foregroundStyle(.secondary)
			}
			Spacer()
			if state.phase == .paired {
				let live = state.problem == nil && state.fleet != nil
				HStack(spacing: 4) {
					Circle().fill(live ? Color.green : Color.orange).frame(width: 6, height: 6)
					Text(live ? "Live" : "Offline").font(.caption2.weight(.semibold))
				}
				.padding(.horizontal, 7).padding(.vertical, 3)
				.background(Capsule().fill((live ? Color.green : Color.orange).opacity(0.15)))
				.foregroundStyle(live ? Color.green : Color.orange)
			}
		}
	}

	private var subtitle: String {
		switch state.phase {
		case .unpaired: "Not connected"
		case .pairing: "Waiting for approval"
		case .paired: state.fleet.map { [$0.device.name, state.worker?.host()].compactMap { $0 }.joined(separator: " · ") } ?? "Connecting…"
		}
	}

	private func totals(_ fleet: Fleet) -> some View {
		HStack(spacing: 0) {
			Figure(value: fleet.running, label: "Running", tint: .blue)
			Divider().frame(height: 28)
			Figure(value: fleet.waiting.count, label: "Waiting", tint: .orange)
			Divider().frame(height: 28)
			Figure(value: fleet.online, label: "of \(fleet.boxes.count) online", tint: .green)
		}
		.padding(.vertical, 8)
		.panel(padding: 0)
	}

	@ViewBuilder private func thisMac(_ fleet: Fleet) -> some View {
		HStack(spacing: 10) {
			Image(systemName: "laptopcomputer")
				.font(.system(size: 17)).foregroundStyle(.secondary).frame(width: 24)
			if let box = state.myBox {
				VStack(alignment: .leading, spacing: 1) {
					Text("This Mac").font(.subheadline.weight(.semibold))
					Text(!box.online ? "\(box.name) is offline" : box.paused ? "Paused · finishes what runs" : "Taking jobs as \(box.name)")
						.font(.caption).foregroundStyle(.secondary)
				}
				Spacer()
				Toggle("Available", isOn: Binding(get: { !box.paused }, set: { state.setAvailable($0) }))
					.toggleStyle(.switch).labelsHidden().controlSize(.small)
					.disabled(state.busy || !box.online)
			} else {
				VStack(alignment: .leading, spacing: 1) {
					Text("This Mac").font(.subheadline.weight(.semibold))
					Text("Which box runs here?").font(.caption).foregroundStyle(.secondary)
				}
				Spacer()
				Picker("Box", selection: Binding(get: { state.thisBox }, set: { state.thisBox = $0 })) {
					Text("None").tag("")
					ForEach(fleet.boxes) { Text($0.name).tag($0.name) }
				}
				.labelsHidden().fixedSize().controlSize(.small)
			}
		}
		.panel()
	}

	private func boxes(_ fleet: Fleet) -> some View {
		VStack(alignment: .leading, spacing: 6) {
			SectionTitle(text: "Boxes")
			VStack(spacing: 0) {
				ForEach(Array(fleet.boxes.enumerated()), id: \.element.id) { index, box in
					if index > 0 { Divider().padding(.leading, 22) }
					BoxRow(box: box, isThisMac: box.name == state.thisBox)
				}
			}
			.panel(padding: 4)
		}
	}

	private func waiting(_ fleet: Fleet) -> some View {
		VStack(alignment: .leading, spacing: 6) {
			SectionTitle(text: "Waiting")
			VStack(alignment: .leading, spacing: 5) {
				ForEach(Array(fleet.waiting.enumerated()), id: \.offset) { _, w in
					HStack(alignment: .firstTextBaseline, spacing: 8) {
						Image(systemName: "hourglass").font(.caption).foregroundStyle(.orange).frame(width: 14)
						VStack(alignment: .leading, spacing: 1) {
							Text("\(repoName(w.repo)) · \(w.class)").font(.caption.weight(.medium))
							if let cause = w.cause {
								Text(cause).font(.caption2).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
							}
						}
						Spacer()
						Text(duration(w.seconds)).font(.caption2).foregroundStyle(.secondary).monospacedDigit()
					}
				}
			}
			.panel()
		}
	}

	private func jobs(_ fleet: Fleet) -> some View {
		VStack(alignment: .leading, spacing: 6) {
			SectionTitle(text: "Recent jobs")
			VStack(alignment: .leading, spacing: 7) {
				if fleet.jobs.isEmpty {
					Text("No job has finished yet").font(.caption).foregroundStyle(.tertiary).frame(maxWidth: .infinity)
				}
				ForEach(fleet.jobs.prefix(5)) { job in
					HStack(spacing: 8) {
						Image(systemName: job.result == "succeeded" ? "checkmark.circle.fill" : job.result == "cancelled" ? "minus.circle.fill" : "xmark.circle.fill")
							.foregroundStyle(job.result == "succeeded" ? Color.green : job.result == "cancelled" ? Color.secondary : Color.red)
							.font(.system(size: 13))
						VStack(alignment: .leading, spacing: 0) {
							Text(job.name).font(.caption.weight(.medium)).lineLimit(1)
							Text([repoName(job.repo), job.box, job.memoryPeak.map(gigabytes)].compactMap { $0 }.joined(separator: " · "))
								.font(.caption2).foregroundStyle(.secondary).lineLimit(1)
						}
						Spacer(minLength: 6)
						VStack(alignment: .trailing, spacing: 0) {
							Text(duration(job.durationSeconds)).font(.caption.weight(.medium)).monospacedDigit()
							Text("\(duration(job.secondsSinceFinished)) ago").font(.caption2).foregroundStyle(.tertiary).monospacedDigit()
						}
					}
				}
			}
			.panel()
		}
	}

	private var footer: some View {
		HStack(spacing: 2) {
			if let updated = state.updated, state.phase == .paired {
				Text("Updated \(updated, style: .relative) ago").font(.caption2).foregroundStyle(.tertiary)
			}
			Spacer()
			if let worker = state.worker, state.phase == .paired {
				IconButton(symbol: "safari", help: "Open the dashboard") { openURL(worker) }
			}
			IconButton(symbol: "gearshape", help: "Settings") { openWindow(id: "settings"); NSApp.activate() }
			IconButton(symbol: "power", help: "Quit") { NSApp.terminate(nil) }
		}
		.padding(.top, 2)
	}
}

/// One box: its health, the slots of each class and what runs there.
struct BoxRow: View {
	var box: Fleet.Box
	var isThisMac: Bool

	var body: some View {
		VStack(alignment: .leading, spacing: 5) {
			HStack(spacing: 6) {
				StatusDot(health: Health(box))
				Text(box.name).font(.subheadline.weight(.semibold))
				if isThisMac {
					Text("this Mac").font(.caption2.weight(.medium)).foregroundStyle(.secondary)
						.padding(.horizontal, 5).padding(.vertical, 1)
						.background(Capsule().fill(Color.primary.opacity(0.08)))
				}
				Spacer()
				Text(status).font(.caption).foregroundStyle(.secondary).monospacedDigit()
			}
			if box.online {
				HStack(spacing: 12) {
					ForEach(box.classes) { c in
						HStack(spacing: 5) {
							Text(c.name).font(.caption2).foregroundStyle(.secondary)
							Slots(class: c)
						}
					}
					Spacer(minLength: 0)
				}
				.padding(.leading, 18)
				ForEach(box.runners.prefix(3)) { r in
					HStack(spacing: 5) {
						Image(systemName: r.state == "running" ? "play.fill" : "ellipsis")
							.font(.system(size: 7)).foregroundStyle(r.state == "running" ? Color.blue : Color.secondary).frame(width: 10)
						Text(repoName(r.repo)).font(.caption2)
						Spacer()
						Text(duration(r.seconds)).font(.caption2).foregroundStyle(.tertiary).monospacedDigit()
					}
					.padding(.leading, 18)
				}
			}
		}
		.padding(.horizontal, 6).padding(.vertical, 7)
	}

	private var status: String {
		guard box.online else { return "offline \(duration(box.secondsSinceSeen))" }
		if box.paused { return "paused" }
		return "\(gigabytes(box.disk.free)) free · \(box.version)"
	}
}

func repoName(_ repo: String) -> String {
	String(repo.split(separator: "/").last ?? Substring(repo))
}
