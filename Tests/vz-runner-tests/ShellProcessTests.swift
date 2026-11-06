import XCTest
@testable import vz_runner

final class ShellProcessTests: XCTestCase {
    // A child that writes more than the pipe buffer (64 KiB on macOS) must
    // not deadlock: waitUntilExit before draining the pipe had the parent
    // waiting for an exit the blocked child could never reach.
    func testShellDrainsLargeOutput() {
        let expectation = expectation(description: "shell returns")
        var out = ""
        DispatchQueue.global().async {
            out = shell("/bin/sh", "-c", "printf 'a%.0s' $(seq 1 200000)")
            expectation.fulfill()
        }
        wait(for: [expectation], timeout: 15)
        XCTAssertEqual(out.count, 200000)
    }

    func testShellCapturesSmallOutput() {
        XCTAssertEqual(shell("/bin/echo", "hello").trimmingCharacters(in: .whitespacesAndNewlines), "hello")
    }
}
