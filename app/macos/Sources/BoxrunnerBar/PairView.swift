import AppKit
import SwiftUI

/// Connecting this Mac: the Worker's address, then the code to approve in the browser.
struct PairView: View {
	@Bindable var state: AppState
	@State private var worker = ""
	@State private var copied = false
	private let deviceName = Host.current().localizedName ?? "Mac"

	var body: some View {
		switch state.phase {
		case .pairing(let code, let url): waiting(code: code, url: url)
		default: connect
		}
	}

	private var connect: some View {
		VStack(alignment: .leading, spacing: 10) {
			VStack(alignment: .leading, spacing: 3) {
				Text("Connect this Mac").font(.headline)
				Text("See every box and job, and switch this Mac in and out of the fleet.")
					.font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
			}
			TextField("https://boxrunner.example.workers.dev", text: $worker)
				.textFieldStyle(.roundedBorder)
				.controlSize(.large)
				.onSubmit(start)
			Button(action: start) {
				Text("Connect").frame(maxWidth: .infinity)
			}
			.buttonStyle(.borderedProminent)
			.controlSize(.large)
			.keyboardShortcut(.defaultAction)
			.disabled(worker.isEmpty)
			Text("Pairs as \(deviceName). You approve it on the dashboard.")
				.font(.caption).foregroundStyle(.tertiary).fixedSize(horizontal: false, vertical: true)
		}
		.onAppear { worker = state.worker?.absoluteString ?? "" }
	}

	private func waiting(code: String, url: URL) -> some View {
		VStack(spacing: 12) {
			Text("Approve in your browser").font(.headline)
			Text(code)
				.font(.system(size: 30, weight: .semibold, design: .monospaced))
				.tracking(3)
				.textSelection(.enabled)
				.padding(.vertical, 12)
			.frame(maxWidth: .infinity)
			.panel(padding: 0)
			HStack(spacing: 6) {
				ProgressView().controlSize(.small)
				Text("Waiting until you press Pair on the dashboard").font(.caption).foregroundStyle(.secondary)
			}
			HStack {
				Button(copied ? "Copied" : "Copy code") {
					NSPasteboard.general.clearContents()
					NSPasteboard.general.setString(code, forType: .string)
					copied = true
				}
				Button("Open page") { NSWorkspace.shared.open(url) }
				Spacer()
				Button("Cancel", role: .cancel) { state.cancelPairing() }
			}
			.controlSize(.small)
		}
	}

	private func start() {
		guard !worker.isEmpty else { return }
		state.pair(worker: worker, deviceName: deviceName)
	}
}
