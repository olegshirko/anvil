import XCTest
@testable import vz_runner

final class AcceptLoopTests: XCTestCase {
    // A closed listener ends the loop; everything else is retried, and
    // descriptor exhaustion must not turn into a busy loop.
    func testAcceptErrorClassification() {
        XCTAssertTrue(acceptShouldStop(EBADF))
        XCTAssertTrue(acceptShouldStop(EINVAL))
        XCTAssertFalse(acceptShouldStop(EINTR))
        XCTAssertFalse(acceptShouldStop(ECONNABORTED))

        let start = Date()
        XCTAssertFalse(acceptShouldStop(EMFILE))
        XCTAssertGreaterThanOrEqual(Date().timeIntervalSince(start), 0.09, "EMFILE must back off")
    }
}
