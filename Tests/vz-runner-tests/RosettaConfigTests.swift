import XCTest
@testable import vz_runner

final class RosettaConfigTests: XCTestCase {
    // Opt-in only: nothing but ANVIL_ROSETTA=1 turns it on.
    func testRosettaIsOptIn() {
        XCTAssertFalse(rosettaRequested(environment: [:]))
        XCTAssertFalse(rosettaRequested(environment: ["ANVIL_ROSETTA": "0"]))
        XCTAssertFalse(rosettaRequested(environment: ["ANVIL_ROSETTA": "yes"]))
        XCTAssertTrue(rosettaRequested(environment: ["ANVIL_ROSETTA": "1"]))
    }
}
