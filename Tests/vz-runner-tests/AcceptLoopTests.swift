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

final class DoctorTests: XCTestCase {
    func testDefaultRouteInterface() {
        let vpn = """
           route to: default
        destination: default
               mask: default
          interface: utun29
              flags: <UP,DONE,CLONING,STATIC>
        """
        XCTAssertEqual(defaultRouteInterface(fromRouteOutput: vpn), "utun29")
        XCTAssertEqual(defaultRouteInterface(fromRouteOutput: "  gateway: 10.0.0.1\n  interface: en0\n"), "en0")
        XCTAssertNil(defaultRouteInterface(fromRouteOutput: "route: writing to routing socket: not in table"))
    }
}
