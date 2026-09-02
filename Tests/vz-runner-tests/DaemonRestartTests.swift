import XCTest
@testable import vz_runner

final class DaemonRestartTests: XCTestCase {
    func testRestartDelayExponentialUpToCap() {
        // attempt is 0-based: 1s, 2s, 4s, ..., capped at 60s.
        XCTAssertEqual(daemonRestartDelay(attempt: 0), 1)
        XCTAssertEqual(daemonRestartDelay(attempt: 1), 2)
        XCTAssertEqual(daemonRestartDelay(attempt: 2), 4)
        XCTAssertEqual(daemonRestartDelay(attempt: 5), 32)
        XCTAssertEqual(daemonRestartDelay(attempt: 6), 60, "1<<6=64 capped at 60")
        XCTAssertEqual(daemonRestartDelay(attempt: 100), 60, "long crash-loops stay at the cap")
    }

    func testRestartDelayNegativeAttemptSafe() {
        XCTAssertEqual(daemonRestartDelay(attempt: -3), 1, "negative attempt clamps to the base delay")
    }
}
