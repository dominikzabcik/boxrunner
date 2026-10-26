import Testing
@testable import BoxrunnerBar

@Test func namesAHostnameAsTheAgentWould() {
	#expect(LocalBoxes.boxName("Dominik’s MacBook Pro") == "dominik-s-macbook-pro")
	#expect(LocalBoxes.boxName("swarmnet-02") == "swarmnet-02")
}

@Test func picksTheOneBoxThatRunsHere() {
	#expect(LocalBoxes.pick(from: ["agent-runner-1", "swarmnet-02"], local: ["swarmnet-02", "br-two", "dominik-s-mac"]) == "swarmnet-02")
}

@Test func picksNothingWhenNoneOrSeveralRunHere() {
	#expect(LocalBoxes.pick(from: ["agent-runner-1"], local: ["swarmnet-02"]) == nil)
	#expect(LocalBoxes.pick(from: ["a", "b"], local: ["a", "b"]) == nil)
}
