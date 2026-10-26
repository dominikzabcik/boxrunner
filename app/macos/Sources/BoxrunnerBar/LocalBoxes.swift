import Foundation

/// The names a box on this Mac could report: the Mac's own hostname, since a
/// box defaults to it, and each Linux VM that OrbStack or Lima runs here, since
/// the agent runs on Linux.
enum LocalBoxes {
	static func names() async -> Set<String> {
		var names: Set<String> = []
		if let host = Host.current().localizedName { names.insert(boxName(host)) }
		names.insert(boxName(ProcessInfo.processInfo.hostName.split(separator: ".").first.map(String.init) ?? ""))
		for (tool, args) in [("orbctl", ["list", "-q", "-r"]), ("limactl", ["list", "-q"])] {
			names.formUnion(await lines(tool, args).map(boxName))
		}
		names.remove("")
		return names
	}

	/// Which of the fleet's boxes runs here, when exactly one does.
	static func pick(from fleet: [String], local: Set<String>) -> String? {
		let here = fleet.filter(local.contains)
		return here.count == 1 ? here[0] : nil
	}

	/// The name a box gets from a hostname: lower case, [a-z0-9-].
	static func boxName(_ raw: String) -> String {
		let mapped = raw.lowercased().map { $0.isASCII && ($0.isLetter || $0.isNumber) ? $0 : "-" }
		return String(mapped).split(separator: "-").joined(separator: "-")
	}

	private static func lines(_ tool: String, _ args: [String]) async -> [String] {
		let path = ["/usr/local/bin", "/opt/homebrew/bin"].map { "\($0)/\(tool)" }.first(where: FileManager.default.isExecutableFile)
		guard let path else { return [] }
		return await Task.detached {
			let process = Process()
			process.executableURL = URL(filePath: path)
			process.arguments = args
			let out = Pipe()
			process.standardOutput = out
			process.standardError = FileHandle.nullDevice
			guard (try? process.run()) != nil else { return [] }
			let data = out.fileHandleForReading.readDataToEndOfFile()
			process.waitUntilExit()
			return String(decoding: data, as: UTF8.self).split(whereSeparator: \.isNewline).map(String.init)
		}.value
	}
}
