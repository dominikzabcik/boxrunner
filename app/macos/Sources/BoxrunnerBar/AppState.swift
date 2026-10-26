import AppKit
import Foundation
import Observation

/// Everything the menu shows, refreshed every 10 seconds while paired.
@MainActor @Observable
final class AppState {
	enum Phase: Equatable {
		/// No Worker or no device token yet.
		case unpaired
		/// A pairing waits for approval on the dashboard.
		case pairing(code: String, url: URL)
		case paired
	}

	var phase: Phase = .unpaired
	var fleet: Fleet?
	var problem: String?
	var updated: Date?
	var busy = false

	/// The fleet's name in the menu, such as the team calls it.
	var fleetName: String { didSet { defaults.set(fleetName, forKey: "fleetName") } }
	/// The box this Mac runs, whose availability the menu switches.
	var thisBox: String { didSet { defaults.set(thisBox, forKey: "thisBox") } }
	private(set) var worker: URL?

	private let defaults = UserDefaults.standard
	private var poller: Task<Void, Never>?
	private var token: String?
	private var localBoxes: Set<String>?

	init() {
		fleetName = defaults.string(forKey: "fleetName") ?? "boxrunner"
		thisBox = defaults.string(forKey: "thisBox") ?? ""
		worker = defaults.url(forKey: "worker")
		if let worker, let token = Keychain.token(for: worker) {
			self.token = token
			phase = .paired
			startPolling()
		}
	}

	/// A state that shows `fleet` and talks to nobody, for tests.
	init(showing fleet: Fleet, fleetName: String, thisBox: String) {
		self.fleetName = fleetName
		self.thisBox = thisBox
		self.fleet = fleet
		phase = .paired
		updated = Date()
	}

	var client: Client? { worker.map { Client(worker: $0) } }

	/// The box of this Mac in the fleet, once chosen and reported.
	var myBox: Fleet.Box? { fleet?.boxes.first { $0.name == thisBox } }

	/// Starts a pairing: the Worker hands out a code, the browser opens its
	/// approval page, and the app collects its token once someone approves.
	func pair(worker raw: String, deviceName: String) {
		guard let url = URL(string: raw.trimmingCharacters(in: .whitespaces)), url.scheme == "https", url.host != nil else {
			problem = "The Worker’s address starts with https://."
			return
		}
		worker = url
		defaults.set(url, forKey: "worker")
		problem = nil
		Task {
			do {
				let client = Client(worker: url)
				let pairing = try await client.pair(name: deviceName)
				phase = .pairing(code: pairing.code, url: pairing.url)
				NSWorkspace.shared.open(pairing.url)
				let deadline = Date().addingTimeInterval(TimeInterval(pairing.expiresIn))
				while Date() < deadline, case .pairing = phase {
					if let paired = try await client.collect(secret: pairing.secret) {
						Keychain.save(paired.token, for: url)
						token = paired.token
						phase = .paired
						startPolling()
						return
					}
					try await Task.sleep(for: .seconds(2))
				}
				if case .pairing = phase {
					phase = .unpaired
					problem = "Nobody approved the pairing within 10 minutes. Pair again."
				}
			} catch {
				phase = .unpaired
				problem = error.localizedDescription
			}
		}
	}

	func cancelPairing() { phase = .unpaired }

	func unpair() {
		poller?.cancel()
		if let worker { Keychain.delete(for: worker) }
		token = nil
		fleet = nil
		phase = .unpaired
	}

	func refresh() async {
		guard let client, let token else { return }
		do {
			let fleet = try await client.fleet(token: token)
			self.fleet = fleet
			updated = Date()
			problem = nil
			await findThisBox(in: fleet)
		} catch let failure as Client.Failure where failure.status == 401 {
			unpair()
			problem = failure.message
		} catch {
			problem = error.localizedDescription
		}
	}

	/// Available: no limit on this Mac's box. Away: every class limited to 0,
	/// so the box finishes its running jobs and takes no new one.
	func setAvailable(_ available: Bool) {
		guard let client, let token, let box = myBox else { return }
		busy = true
		Task {
			defer { busy = false }
			do {
				for c in box.classes {
					try await client.limit(token: token, box: box.name, className: c.name, max: available ? nil : 0)
				}
				await refresh()
			} catch {
				problem = error.localizedDescription
			}
		}
	}

	/// Chooses this Mac's box on its own when none is chosen yet and exactly
	/// one box of the fleet runs here, so the switch shows without a setting.
	private func findThisBox(in fleet: Fleet) async {
		guard thisBox.isEmpty else { return }
		if localBoxes == nil { localBoxes = await LocalBoxes.names() }
		if let found = LocalBoxes.pick(from: fleet.boxes.map(\.name), local: localBoxes ?? []) { thisBox = found }
	}

	private func startPolling() {
		poller?.cancel()
		poller = Task {
			while !Task.isCancelled {
				await refresh()
				try? await Task.sleep(for: .seconds(10))
			}
		}
	}
}
