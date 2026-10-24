import AppKit
import SwiftUI

/// Connecting this Mac, as a prompt: the Worker's address, then the code to
/// approve in the browser.
struct PairView: View {
	@Bindable var state: AppState
	@State private var worker = ""
	@State private var copied = false
	@FocusState private var focused: Bool
	private let deviceName = Host.current().localizedName ?? "Mac"

	var body: some View {
		switch state.phase {
		case .pairing(let code, let url): waiting(code: code, url: url)
		default: connect
		}
	}

	private var connect: some View {
		Block(title: "connect", accent: Console.accent) {
			Text("pair this mac with your worker to see the fleet and switch this mac in and out of it.")
				.foregroundStyle(Console.muted).font(Console.font(11))
				.fixedSize(horizontal: false, vertical: true)
			HStack(spacing: 6) {
				Text("❯").foregroundStyle(Console.accent).bold()
				TextField("", text: $worker, prompt: Text("https://your-worker.workers.dev").foregroundStyle(Console.faint))
					.textFieldStyle(.plain)
					.font(Console.font(12))
					.focused($focused)
					.onSubmit(start)
			}
			.padding(.horizontal, 8).padding(.vertical, 7)
			.overlay(Rectangle().strokeBorder(focused ? Console.accent : Console.line, lineWidth: 1))
			HStack {
				Text("as \(deviceName)").foregroundStyle(Console.faint).font(Console.font(10)).lineLimit(1)
				Spacer()
				KeyButton(title: "connect", key: "⏎", tint: Console.accent, filled: true, action: start)
					.disabled(worker.isEmpty)
					.opacity(worker.isEmpty ? 0.5 : 1)
			}
		}
		.onAppear {
			worker = state.worker?.absoluteString ?? ""
			focused = true
		}
	}

	private func waiting(code: String, url: URL) -> some View {
		Block(title: "approve in your browser", accent: Console.accent) {
			Text(code)
				.font(Console.font(32, .bold))
				.tracking(4)
				.foregroundStyle(Console.accent)
				.textSelection(.enabled)
				.frame(maxWidth: .infinity)
				.padding(.vertical, 8)
			TimelineView(.periodic(from: .now, by: 0.12)) { context in
				let frames = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"]
				let i = Int(context.date.timeIntervalSinceReferenceDate / 0.12) % frames.count
				(Text(frames[i]).foregroundColor(Console.accent) + Text(" waiting until you press pair on the dashboard").foregroundColor(Console.muted))
					.font(Console.font(11))
			}
			HStack(spacing: 2) {
				KeyButton(title: copied ? "copied" : "copy", key: "⌘C") {
					NSPasteboard.general.clearContents()
					NSPasteboard.general.setString(code, forType: .string)
					copied = true
				}
				.keyboardShortcut("c")
				KeyButton(title: "open page", key: "⌘O") { NSWorkspace.shared.open(url) }
					.keyboardShortcut("o")
				Spacer()
				KeyButton(title: "cancel", key: "esc", tint: Console.muted) { state.cancelPairing() }
					.keyboardShortcut(.cancelAction)
			}
		}
	}

	private func start() {
		guard !worker.isEmpty else { return }
		state.pair(worker: worker, deviceName: deviceName)
	}
}
