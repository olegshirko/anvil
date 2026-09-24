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

final class SignalTests: XCTestCase {
    // The handler runs on the main queue, outside signal context.
    func testOnSignalRunsHandlerOnMainQueue() {
        let fired = expectation(description: "handler")
        onSignal(SIGUSR2) {
            XCTAssertTrue(Thread.isMainThread)
            fired.fulfill()
        }
        kill(getpid(), SIGUSR2)
        wait(for: [fired], timeout: 2)
    }
}
