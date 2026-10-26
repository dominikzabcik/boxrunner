import Foundation
import Testing
@testable import BoxrunnerBar

@Test func readsTheVersionFromTheLatestReleaseRedirect() {
	#expect(Release.version(fromLocation: "https://github.com/dominikzabcik/boxrunner/releases/tag/v0.5.1") == "0.5.1")
	#expect(Release.version(fromLocation: "/dominikzabcik/boxrunner/releases/tag/v12.0.30") == "12.0.30")
}

@Test func rejectsRedirectsThatAreNoRelease() {
	#expect(Release.version(fromLocation: "https://github.com/dominikzabcik/boxrunner/releases") == nil)
	#expect(Release.version(fromLocation: "https://github.com/dominikzabcik/boxrunner/releases/tag/v0.5") == nil)
	#expect(Release.version(fromLocation: "https://github.com/dominikzabcik/boxrunner/releases/tag/v0.5.1-rc.1") == nil)
	#expect(Release.version(fromLocation: "https://github.com/dominikzabcik/boxrunner/releases/tag/vx.y.z") == nil)
	#expect(Release.version(fromLocation: "") == nil)
}

@Test func comparesVersionsByTheirNumbers() {
	#expect(Release.compare("0.2.1", "0.2.1") == 0)
	#expect(Release.compare("0.2.1", "0.10.0") == -1)
	#expect(Release.compare("1.0.0", "0.99.99") == 1)
	#expect(Release.compare("v0.3.0", "0.3.0") == 0)
	#expect(Release.compare("0.3.0+build5", "0.3.0") == 0)
	#expect(Release.compare("0.3", "0.3.0") == 0)
}

@Test func putsAPreReleaseBeforeItsRelease() {
	#expect(Release.compare("0.3.0-rc.1", "0.3.0") == -1)
	#expect(Release.compare("0.3.0", "0.3.0-rc.1") == 1)
	#expect(Release.compare("0.3.0-rc.1", "0.3.0-rc.2") == -1)
	#expect(Release.compare("0.2.9", "0.3.0-rc.1") == -1)
}

@Test func updatesOnlyToANewerVersion() {
	#expect(Release.isNewer("0.5.1", than: "0.5.0"))
	#expect(!Release.isNewer("0.5.0", than: "0.5.0"))
	#expect(!Release.isNewer("0.4.9", than: "0.5.0"))
	#expect(!Release.isNewer("", than: "0.5.0"))
	#expect(!Release.isNewer("0.5.1", than: ""))
}

private let sums = """
0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7  boxrunner-linux-amd64
11112222333344445555666677778888999900001111222233334444AAAABBBB  boxrunner-linux-arm64
aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999 *boxrunner-macos.zip
"""

@Test func findsAFilesHashInChecksums() {
	#expect(Checksums.hash(of: "boxrunner-linux-amd64", in: sums) == "0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7")
	#expect(Checksums.hash(of: "boxrunner-linux-arm64", in: sums) == "11112222333344445555666677778888999900001111222233334444aaaabbbb")
	#expect(Checksums.hash(of: "boxrunner-macos.zip", in: sums) == "aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999")
}

@Test func findsNoHashForAnUnlistedOrBrokenLine() {
	#expect(Checksums.hash(of: "boxrunner-macos.zip", in: "") == nil)
	#expect(Checksums.hash(of: "boxrunner-macos", in: sums) == nil)
	#expect(Checksums.hash(of: "boxrunner-macos.zip", in: "abc123  boxrunner-macos.zip\n") == nil)
	#expect(Checksums.hash(of: "boxrunner-macos.zip", in: "not a checksum line\r\n") == nil)
}

@Test func hashesAFileInChunks() throws {
	let file = FileManager.default.temporaryDirectory.appending(path: "boxrunner-sha-\(UUID().uuidString)")
	defer { try? FileManager.default.removeItem(at: file) }
	try Data("abc".utf8).write(to: file)
	#expect(try Checksums.sha256(of: file) == "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	try Data(repeating: 0x61, count: (1 << 20) + 1).write(to: file)
	#expect(try Checksums.sha256(of: file) == "4a3f0c0c213adea174f9a3d4c13177315b588bdb2e9c1012d3d0bf0453ca0f6a")
}
