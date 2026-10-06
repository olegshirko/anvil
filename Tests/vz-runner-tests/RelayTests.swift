import XCTest
@testable import vz_runner

// Host relays between clients and the guest. pumpBuffered must keep reading
// the guest side while the client is slow (a stalled vsock connection
// stalled the whole device), and every pump must pass a half-close on.
final class RelayTests: XCTestCase {
    private func socketPair() -> (Int32, Int32) {
        var fds: [Int32] = [0, 0]
        XCTAssertEqual(socketpair(AF_UNIX, SOCK_STREAM, 0, &fds), 0)
        for fd in fds {
            var on: Int32 = 1
            setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
        }
        return (fds[0], fds[1])
    }

    private func writeAllBlocking(_ fd: Int32, _ data: [UInt8]) -> Bool {
        var off = 0
        while off < data.count {
            let w = data.withUnsafeBytes { write(fd, $0.baseAddress!.advanced(by: off), data.count - off) }
            if w <= 0 { return false }
            off += w
        }
        return true
    }

    private func readToEOF(_ fd: Int32) -> [UInt8] {
        var out: [UInt8] = []
        var buf = [UInt8](repeating: 0, count: 65536)
        while true {
            let n = read(fd, &buf, buf.count)
            if n <= 0 { return out }
            out.append(contentsOf: buf[0..<n])
        }
    }

    private func payload(_ size: Int) -> [UInt8] {
        (0..<size).map { UInt8(truncatingIfNeeded: $0 &* 31 &+ $0 >> 8) }
    }

    /// The guest side is drained although the client reads nothing yet.
    func testBufferedPumpDrainsWhileClientIsSlow() {
        let (guestPeer, guest) = socketPair()
        let (client, clientPeer) = socketPair()
        let data = payload(8 << 20) // far beyond the socket buffers
        Thread { pumpBuffered(from: guest, to: client) }.start()

        let wrote = expectation(description: "guest side accepted every byte")
        Thread {
            if self.writeAllBlocking(guestPeer, data) { wrote.fulfill() }
            shutdown(guestPeer, SHUT_WR)
        }.start()
        wait(for: [wrote], timeout: 10) // nobody reads clientPeer yet

        XCTAssertEqual(readToEOF(clientPeer), data, "bytes in order, then EOF")
        [guestPeer, guest, client, clientPeer].forEach { close($0) }
    }

    /// Past the limit the pump stops reading: backpressure, not unbounded memory.
    func testBufferedPumpRespectsLimit() {
        let (guestPeer, guest) = socketPair()
        let (client, clientPeer) = socketPair()
        let data = payload(16 << 20)
        Thread { pumpBuffered(from: guest, to: client, limit: 1 << 20) }.start()

        let wrote = expectation(description: "writer finished")
        wrote.isInverted = true
        Thread {
            if self.writeAllBlocking(guestPeer, data) { wrote.fulfill() }
            shutdown(guestPeer, SHUT_WR)
        }.start()
        wait(for: [wrote], timeout: 1)

        XCTAssertEqual(readToEOF(clientPeer), data)
        [guestPeer, guest, client, clientPeer].forEach { close($0) }
    }

    /// A client's half-close reaches the guest side while the reverse
    /// direction keeps flowing.
    func testRelayPassesHalfClose() {
        let (clientPeer, client) = socketPair()
        let (guest, guestPeer) = socketPair()
        Thread { relayBothWays(client, guest, vsock: guest) }.start()

        let input = payload(3 << 20)
        Thread { // client -> guest is not buffered: write while the guest side reads
            _ = self.writeAllBlocking(clientPeer, input)
            shutdown(clientPeer, SHUT_WR)
        }.start()
        XCTAssertEqual(readToEOF(guestPeer), input, "stdin then EOF on the guest side")

        let output = payload(1 << 20).reversed().map { $0 }
        XCTAssertTrue(writeAllBlocking(guestPeer, output))
        shutdown(guestPeer, SHUT_WR)
        XCTAssertEqual(readToEOF(clientPeer), output, "output still flows after the half-close")
        [clientPeer, client, guest, guestPeer].forEach { close($0) }
    }
}
