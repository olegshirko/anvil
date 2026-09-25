import XCTest
@testable import vz_runner

final class GuestConnectionsTests: XCTestCase {
    func testLimiterCapsConcurrentConnections() {
        let limiter = ConnectionLimiter(limit: 2)
        XCTAssertTrue(limiter.tryAcquire())
        XCTAssertTrue(limiter.tryAcquire())
        XCTAssertFalse(limiter.tryAcquire(), "third connection over the cap")
        limiter.release()
        XCTAssertTrue(limiter.tryAcquire(), "a slot frees up on release")
        XCTAssertEqual(limiter.count, 2)
    }

    // A peer that never sends its handshake must not hold the thread forever.
    func testReceiveTimeoutBoundsHandshake() throws {
        var pair: [Int32] = [0, 0]
        XCTAssertEqual(socketpair(AF_UNIX, SOCK_STREAM, 0, &pair), 0)
        defer { close(pair[0]); close(pair[1]) }
        let start = Date()
        var buf = [UInt8](repeating: 0, count: 4)
        let n = try withReceiveTimeout(pair[1], seconds: 1) { read(pair[1], &buf, 4) }
        XCTAssertEqual(n, -1)
        XCTAssertLessThan(Date().timeIntervalSince(start), 3)
    }

    // One direction ending does not stop the other (half-close).
    func testRelayKeepsOtherDirectionAfterHalfClose() {
        var guest: [Int32] = [0, 0], upstream: [Int32] = [0, 0]
        XCTAssertEqual(socketpair(AF_UNIX, SOCK_STREAM, 0, &guest), 0)
        XCTAssertEqual(socketpair(AF_UNIX, SOCK_STREAM, 0, &upstream), 0)
        let done = expectation(description: "relay ends")
        Thread { relayBothWays(guest[1], upstream[1]); done.fulfill() }.start()
        shutdown(guest[0], SHUT_WR) // client done sending
        var buf = [UInt8](repeating: 0, count: 8)
        XCTAssertEqual(read(upstream[0], &buf, 8), 0)
        XCTAssertEqual(write(upstream[0], "late", 4), 4) // response still flows back
        XCTAssertEqual(read(guest[0], &buf, 8), 4)
        shutdown(upstream[0], SHUT_WR)
        wait(for: [done], timeout: 2)
        for fd in guest + upstream { close(fd) }
    }
}
