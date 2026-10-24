import ServiceManagement
import SwiftUI

/// Pairing and the few settings: the Worker, the fleet's name, this Mac's box.
struct SettingsView: View {
	@Bindable var state: AppState
	@State private var worker = ""
	@State private var deviceName = Host.current().localizedName ?? "Mac"
	@State private var openAtLogin = SMAppService.mainApp.status == .enabled

	var body: some View {
		Form {
			Section {
				TextField("Fleet name", text: $state.fleetName)
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
			case .unpaired:
				Section("Worker") {
					TextField("Address", text: $worker, prompt: Text("https://boxrunner.example.workers.dev"))
					TextField("This Mac’s name", text: $deviceName)
					Button("Pair") { state.pair(worker: worker, deviceName: deviceName) }
						.keyboardShortcut(.defaultAction)
						.disabled(worker.isEmpty || deviceName.isEmpty)
				}
			case .pairing(let code, let url):
				Section("Approve in your browser") {
					Text(code).font(.system(.largeTitle, design: .monospaced).weight(.semibold)).textSelection(.enabled)
					Text("Check the dashboard shows the same code, then press Pair there. This window updates on its own.")
						.font(.callout).foregroundStyle(.secondary)
					HStack {
						Link("Open the page again", destination: url)
						Spacer()
						Button("Cancel") { state.cancelPairing() }
					}
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
		.onAppear { worker = state.worker?.absoluteString ?? "" }
	}
}
