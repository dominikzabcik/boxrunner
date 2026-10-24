import Foundation
import Security

/// The Worker's device API (docs/api.md#paired-devices).
struct Client: Sendable {
	var worker: URL
	var session: URLSession = .shared

	struct Pairing: Decodable, Sendable {
		var code: String
		var secret: String
		var url: URL
		var expiresIn: Int
	}

	struct Paired: Decodable, Sendable {
		var token: String
		var device: Fleet.Device
	}

	struct Failure: LocalizedError {
		var status: Int
		var message: String
		var errorDescription: String? { message }
	}

	func pair(name: String) async throws -> Pairing {
		try await send("POST", "/v1/pair", body: ["name": name], expect: [201])
	}

	/// nil while nobody approved the pairing yet.
	func collect(secret: String) async throws -> Paired? {
		let (data, status) = try await raw("POST", "/v1/pair/token", body: ["secret": secret], token: nil)
		if status == 202 { return nil }
		try check(data, status, expect: [200])
		return try JSONDecoder().decode(Paired.self, from: data)
	}

	func fleet(token: String) async throws -> Fleet {
		try await send("GET", "/v1/fleet", token: token, expect: [200])
	}

	/// Sets a class's limit on a box; nil removes it.
	func limit(token: String, box: String, className: String, max: Int?) async throws {
		let body: [String: Any] = ["box": box, "class": className, "max": max as Any? ?? NSNull()]
		let (data, status) = try await raw("POST", "/v1/fleet/limit", body: body, token: token)
		try check(data, status, expect: [200])
	}

	private func send<T: Decodable>(_ method: String, _ path: String, body: [String: Any]? = nil, token: String? = nil, expect: Set<Int>) async throws -> T {
		let (data, status) = try await raw(method, path, body: body, token: token)
		try check(data, status, expect: expect)
		return try JSONDecoder().decode(T.self, from: data)
	}

	private func raw(_ method: String, _ path: String, body: [String: Any]?, token: String?) async throws -> (Data, Int) {
		var request = URLRequest(url: worker.appending(path: path), timeoutInterval: 15)
		request.httpMethod = method
		if let token { request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
		if let body {
			request.setValue("application/json", forHTTPHeaderField: "Content-Type")
			request.httpBody = try JSONSerialization.data(withJSONObject: body)
		}
		let (data, response) = try await session.data(for: request)
		return (data, (response as? HTTPURLResponse)?.statusCode ?? 0)
	}

	private func check(_ data: Data, _ status: Int, expect: Set<Int>) throws {
		guard !expect.contains(status) else { return }
		let message = (try? JSONDecoder().decode([String: String].self, from: data))?["message"] ?? "The Worker answered \(status)."
		throw Failure(status: status, message: message)
	}
}

/// The device token, in the login keychain, per Worker.
enum Keychain {
	private static let service = "boxrunner menu bar"

	static func token(for worker: URL) -> String? {
		var item: CFTypeRef?
		let query: [String: Any] = [
			kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
			kSecAttrAccount as String: worker.absoluteString, kSecReturnData as String: true,
		]
		guard SecItemCopyMatching(query as CFDictionary, &item) == errSecSuccess, let data = item as? Data else { return nil }
		return String(data: data, encoding: .utf8)
	}

	static func save(_ token: String, for worker: URL) {
		delete(for: worker)
		let item: [String: Any] = [
			kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
			kSecAttrAccount as String: worker.absoluteString, kSecValueData as String: Data(token.utf8),
			kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
		]
		SecItemAdd(item as CFDictionary, nil)
	}

	static func delete(for worker: URL) {
		let query: [String: Any] = [
			kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
			kSecAttrAccount as String: worker.absoluteString,
		]
		SecItemDelete(query as CFDictionary)
	}
}
