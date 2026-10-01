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

    // An upstream whose output grows with its input (`docker exec -i c xxd`)
    // must not deadlock the relay once both directions' buffers fill.
    func testRelayDoesNotDeadlockOnAmplifyingUpstream() {
        var client: [Int32] = [0, 0], upstream: [Int32] = [0, 0]
        XCTAssertEqual(socketpair(AF_UNIX, SOCK_STREAM, 0, &client), 0)
        XCTAssertEqual(socketpair(AF_UNIX, SOCK_STREAM, 0, &upstream), 0)
        let relayDone = expectation(description: "relay ends")
        Thread { relayBothWays(client[1], upstream[1]); relayDone.fulfill() }.start()

        let input = 4 << 20
        // Upstream: every byte read comes back twice; blocks while writing.
        Thread {
            var buf = [UInt8](repeating: 0, count: 16384)
            while true {
                let n = read(upstream[0], &buf, buf.count)
                if n <= 0 { break }
                for _ in 0..<2 {
                    var off = 0
                    while off < n {
                        let w = buf.withUnsafeBytes { write(upstream[0], $0.baseAddress!.advanced(by: off), n - off) }
                        if w <= 0 { return }
                        off += w
                    }
                }
            }
            shutdown(upstream[0], SHUT_WR)
        }.start()
        // Client: writes its whole input while a second thread reads.
        Thread {
            let chunk = [UInt8](repeating: 0x61, count: 16384)
            var sent = 0
            while sent < input {
                let w = chunk.withUnsafeBytes { write(client[0], $0.baseAddress!, min(chunk.count, input - sent)) }
                if w <= 0 { return }
                sent += w
            }
            shutdown(client[0], SHUT_WR)
        }.start()
        let got = expectation(description: "all output received")
        Thread {
            var buf = [UInt8](repeating: 0, count: 65536)
            var total = 0
            while true {
                let n = read(client[0], &buf, buf.count)
                if n <= 0 { break }
                total += n
            }
            XCTAssertEqual(total, 2 * input)
            got.fulfill()
        }.start()
        wait(for: [got, relayDone], timeout: 20)
        for fd in client + upstream { close(fd) }
    }
}
