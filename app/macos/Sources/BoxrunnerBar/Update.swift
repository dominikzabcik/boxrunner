import CryptoKit
import Foundation

/// Reading boxrunner's releases on github.com, the way the agent does.
enum Release {
	static let repo = "dominikzabcik/boxrunner"
	/// The release asset the app updates from, listed in the release's checksums.txt.
	static let asset = "boxrunner-macos.zip"

	/// The version a latest-release page redirects to, such as `0.5.1` for
	/// `https://github.com/…/releases/tag/v0.5.1`.
	static func version(fromLocation location: String) -> String? {
		guard let range = location.range(of: "/releases/tag/v", options: .backwards) else { return nil }
		let version = String(location[range.upperBound...])
		let parts = version.split(separator: ".", omittingEmptySubsequences: false)
		guard parts.count == 3, parts.allSatisfy({ !$0.isEmpty && $0.allSatisfy(\.isASCII) && $0.allSatisfy(\.isNumber) }) else { return nil }
		return version
	}

	/// Orders two versions such as 0.2.1 and 0.3.0-rc.1 by their numbers; a
	/// pre-release comes before the release it leads to.
	static func compare(_ a: String, _ b: String) -> Int {
		let (an, apre) = parse(a), (bn, bpre) = parse(b)
		for (x, y) in zip(an, bn) where x != y { return x < y ? -1 : 1 }
		switch (apre, bpre) {
		case (apre, bpre) where apre == bpre: return 0
		case ("", _): return 1
		case (_, ""): return -1
		default: return apre < bpre ? -1 : 1
		}
	}

	/// Whether `a` comes after `b`. An empty version is never newer, and nothing is newer than one.
	static func isNewer(_ a: String, than b: String) -> Bool {
		!a.isEmpty && !b.isEmpty && compare(a, b) > 0
	}

	private static func parse(_ version: String) -> (numbers: [Int], pre: String) {
		var v = Substring(version)
		if v.hasPrefix("v") { v = v.dropFirst() }
		v = v.split(separator: "+", maxSplits: 1, omittingEmptySubsequences: false)[0]
		let halves = v.split(separator: "-", maxSplits: 1, omittingEmptySubsequences: false)
		let numbers = halves[0].split(separator: ".", maxSplits: 2, omittingEmptySubsequences: false).map { Int($0) ?? 0 }
		return (numbers + Array(repeating: 0, count: max(0, 3 - numbers.count)), halves.count > 1 ? String(halves[1]) : "")
	}
}

/// The release's checksums.txt, as `sha256sum` writes it: a hash, two spaces
/// (or a space and `*`) and the file's name.
enum Checksums {
	static func hash(of file: String, in text: String) -> String? {
		for line in text.split(whereSeparator: \.isNewline) {
			let fields = line.split(separator: " ", maxSplits: 1, omittingEmptySubsequences: true)
			guard fields.count == 2 else { continue }
			var name = fields[1].drop { $0 == " " }
			if name.hasPrefix("*") { name = name.dropFirst() }
			let hash = fields[0].lowercased()
			if name == file, hash.count == 64, hash.allSatisfy(\.isHexDigit) { return hash }
		}
		return nil
	}

	static func sha256(of file: URL) throws -> String {
		let handle = try FileHandle(forReadingFrom: file)
		defer { try? handle.close() }
		var hasher = SHA256()
		while let chunk = try handle.read(upToCount: 1 << 20), !chunk.isEmpty { hasher.update(data: chunk) }
		return hasher.finalize().map { String(format: "%02x", $0) }.joined()
	}
}

/// Replaces the running app with the latest release: download, check the
/// download against the release's checksums, unpack next to the app and swap.
struct Updater: Sendable {
	struct Failure: LocalizedError {
		var message: String
		var errorDescription: String? { message }
	}

	var source: URL = Updater.defaultSource
	var session = URLSession(configuration: .ephemeral)

	/// github.com, or the address in `BOXRUNNER_UPDATE_SOURCE` to try an update
	/// without publishing one: a server with GitHub's paths, on https or this Mac.
	static var defaultSource: URL {
		if let raw = ProcessInfo.processInfo.environment["BOXRUNNER_UPDATE_SOURCE"], let url = URL(string: raw),
		   url.scheme == "https" || (url.scheme == "http" && ["127.0.0.1", "localhost"].contains(url.host())) {
			return url
		}
		return URL(string: "https://github.com/\(Release.repo)")!
	}

	/// The latest release, from the redirect of its page, which needs no API quota.
	func latest() async throws -> String {
		var request = URLRequest(url: source.appending(path: "releases/latest"), timeoutInterval: 20)
		request.httpMethod = "HEAD"
		let (_, response) = try await session.data(for: request, delegate: StayPut())
		guard let http = response as? HTTPURLResponse, (300..<400).contains(http.statusCode) else {
			throw Failure(message: "the latest release page answered HTTP \((response as? HTTPURLResponse)?.statusCode ?? 0)")
		}
		guard let location = http.value(forHTTPHeaderField: "Location"), let version = Release.version(fromLocation: location) else {
			throw Failure(message: "the latest release page redirected somewhere unexpected")
		}
		return version
	}

	/// Replaces the app at `bundle` with `version`. Nothing changes unless the
	/// download matches its published checksum and is this app at that version;
	/// the old app is deleted only after the swap succeeded.
	func install(version: String, over bundle: URL, identifier: String) async throws {
		let downloads = source.appending(path: "releases/download/v\(version)")
		let sums = try await download(downloads.appending(path: "checksums.txt"))
		defer { try? FileManager.default.removeItem(at: sums) }
		guard let want = Checksums.hash(of: Release.asset, in: String(decoding: try Data(contentsOf: sums), as: UTF8.self)) else {
			throw Failure(message: "checksums.txt of v\(version) has no \(Release.asset)")
		}
		let zip = try await download(downloads.appending(path: Release.asset))
		defer { try? FileManager.default.removeItem(at: zip) }
		let got = try Checksums.sha256(of: zip)
		guard got == want else { throw Failure(message: "the download does not match its checksum, so nothing was installed") }

		// Next to the app, on the same volume, so the swap is one rename.
		let parent = bundle.deletingLastPathComponent()
		let stage = parent.appending(path: ".boxrunner-update-\(UUID().uuidString)")
		try FileManager.default.createDirectory(at: stage, withIntermediateDirectories: false)
		defer { try? FileManager.default.removeItem(at: stage) }
		try await run("/usr/bin/ditto", ["-x", "-k", zip.path, stage.path])
		let apps = try FileManager.default.contentsOfDirectory(at: stage, includingPropertiesForKeys: nil).filter { $0.pathExtension == "app" }
		guard apps.count == 1, let new = Bundle(url: apps[0]) else { throw Failure(message: "the download holds no single app") }
		guard new.bundleIdentifier == identifier,
		      new.infoDictionary?["CFBundleShortVersionString"] as? String == version
		else { throw Failure(message: "the download is not boxrunner \(version)") }
		try await run("/usr/bin/codesign", ["--verify", "--deep", "--strict", apps[0].path])

		_ = try FileManager.default.replaceItemAt(bundle, withItemAt: apps[0])
	}

	/// Starts the app at `bundle` again once this process is gone.
	static func relaunch(_ bundle: URL) throws {
		let helper = Process()
		helper.executableURL = URL(filePath: "/bin/sh")
		helper.arguments = ["-c", "while kill -0 \"$1\" 2>/dev/null; do sleep 0.2; done; exec /usr/bin/open \"$2\"", "sh",
		                    String(ProcessInfo.processInfo.processIdentifier), bundle.path]
		try helper.run()
	}

	private func download(_ url: URL) async throws -> URL {
		let (file, response) = try await session.download(for: URLRequest(url: url, timeoutInterval: 60))
		let status = (response as? HTTPURLResponse)?.statusCode ?? 0
		guard status == 200 else {
			try? FileManager.default.removeItem(at: file)
			throw Failure(message: "\(url.lastPathComponent) answered HTTP \(status)")
		}
		return file
	}

	private func run(_ tool: String, _ arguments: [String]) async throws {
		nonisolated(unsafe) let errors = Pipe()
		try await withCheckedThrowingContinuation { (done: CheckedContinuation<Void, Error>) in
			let process = Process()
			process.executableURL = URL(filePath: tool)
			process.arguments = arguments
			process.standardError = errors
			process.terminationHandler = { finished in
				if finished.terminationStatus == 0 { return done.resume() }
				let said = String(decoding: errors.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
				done.resume(throwing: Failure(message: "\(URL(filePath: tool).lastPathComponent) failed: \(said)"))
			}
			do { try process.run() } catch { done.resume(throwing: error) }
		}
	}
}

/// Stops a request at its redirect, so the redirect's address is the answer.
private final class StayPut: NSObject, URLSessionTaskDelegate {
	func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
	                newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
		completionHandler(nil)
	}
}
