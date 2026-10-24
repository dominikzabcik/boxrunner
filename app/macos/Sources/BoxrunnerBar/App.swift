import SwiftUI

@main
struct BoxrunnerBar: App {
	@State private var state = AppState()

	var body: some Scene {
		MenuBarExtra {
			MenuView(state: state)
		} label: {
			MenuLabel(state: state)
		}
		.menuBarExtraStyle(.window)

		Window("Pair with a Worker", id: "settings") {
			SettingsView(state: state)
		}
		.windowResizability(.contentSize)
	}
}

/// The menu bar item: the fleet's state at a glance, and how many jobs run.
struct MenuLabel: View {
	var state: AppState

	var body: some View {
		let fleet = state.fleet
		let symbol: String = switch true {
		case state.phase != .paired: "square.stack.3d.up.slash"
		case state.myBox?.paused == true: "pause.circle"
		case (fleet?.running ?? 0) > 0: "square.stack.3d.up.fill"
		default: "square.stack.3d.up"
		}
		HStack(spacing: 3) {
			Image(systemName: symbol)
			if let running = fleet?.running, running > 0 {
				Text("\(running)").monospacedDigit()
			}
		}
	}
}
