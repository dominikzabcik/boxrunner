import ServiceManagement
import SwiftUI

/// Pairing and the few settings: the Worker, the fleet's name, this Mac's box.
struct SettingsView: View {
	@Bindable var state: AppState
	@State private var openAtLogin = SMAppService.mainApp.status == .enabled

	var body: some View {
		Form {
			Section {
				TextField("Fleet name", text: $state.fleetName)
				if let version = AppState.version {
					Toggle("Update automatically", isOn: $state.updatesAutomatically)
					LabeledContent("Version", value: version)
				}
				Toggle("Open at login", isOn: $openAtLogin)
					.onChange(of: openAtLogin) { _, on in
						do {
							if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
						} catch {
							state.problem = "Open at login: \(error.localizedDescription)"
							openAtLogin = SMAppService.mainApp.status == .enabled
						}
					}
			}
			switch state.phase {
			case .unpaired, .pairing:
				Section("Worker") {
					Text("Not paired. Open the menu bar item to connect this Mac.").foregroundStyle(.secondary)
				}
			case .paired:
				Section("Paired") {
					LabeledContent("Worker", value: state.worker?.host() ?? "")
					if let device = state.fleet?.device {
						LabeledContent("Device", value: "\(device.name), approved by \(device.by)")
					}
					Picker("This Mac’s box", selection: $state.thisBox) {
						Text("None").tag("")
						ForEach(state.fleet?.boxes ?? []) { Text($0.name).tag($0.name) }
					}
					Button("Unpair", role: .destructive) { state.unpair() }
				}
			}
			if let problem = state.problem {
				Text(problem).foregroundStyle(.orange)
			}
		}
		.formStyle(.grouped)
		.frame(width: 440)
	}
}
