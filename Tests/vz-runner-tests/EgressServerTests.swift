import XCTest
@testable import vz_runner

final class EgressServerTests: XCTestCase {
    func testParseEgressTarget() {
        XCTAssertTrue(parseEgressTarget("registry-1.docker.io:443")! == ("registry-1.docker.io", 443))
        XCTAssertTrue(parseEgressTarget("[2001:db8::1]:5000")! == ("2001:db8::1", 5000))
        XCTAssertNil(parseEgressTarget("no-port"))
        XCTAssertNil(parseEgressTarget(":443"))
        XCTAssertNil(parseEgressTarget("host:0"))
        XCTAssertNil(parseEgressTarget("host:70000"))
    }

    // The guest must not reach services bound to the Mac's localhost.
    func testLoopbackTargetsAreRefused() {
        for target in ["127.0.0.1:22", "localhost:5432", "[::1]:80", "0.0.0.0:80"] {
            switch dialEgressTarget(target, timeoutSeconds: 1) {
            case .success(let fd):
                close(fd)
                XCTFail("\(target) must be refused")
            case .failure(let error):
                XCTAssertTrue(error.message.contains("loopback") || error.message.contains("resolve"),
                              "\(target): \(error.message)")
            }
        }
    }

    func testMappedLoopbackIsForbidden() {
        var a = sockaddr_in6()
        a.sin6_family = sa_family_t(AF_INET6)
        withUnsafeMutableBytes(of: &a.sin6_addr) { raw in
            raw[10] = 0xff; raw[11] = 0xff; raw[12] = 127; raw[15] = 1
        }
        let forbidden = withUnsafePointer(to: &a) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { isForbiddenEgressAddress($0) }
        }
        XCTAssertTrue(forbidden)
    }

    // Bytes flow both ways and a half-close propagates.
    func testRelayBothWays() throws {
        var guest: [Int32] = [0, 0], upstream: [Int32] = [0, 0]
        XCTAssertEqual(socketpair(AF_UNIX, SOCK_STREAM, 0, &guest), 0)
        XCTAssertEqual(socketpair(AF_UNIX, SOCK_STREAM, 0, &upstream), 0)
        let done = expectation(description: "relay ends")
        DispatchQueue.global().async {
            relayBothWays(guest[1], upstream[1])
            done.fulfill()
        }
        XCTAssertEqual(write(guest[0], "ping", 4), 4)
        var buf = [UInt8](repeating: 0, count: 4)
        XCTAssertEqual(read(upstream[0], &buf, 4), 4)
        XCTAssertEqual(String(decoding: buf, as: UTF8.self), "ping")
        XCTAssertEqual(write(upstream[0], "pong", 4), 4)
        XCTAssertEqual(read(guest[0], &buf, 4), 4)
        XCTAssertEqual(String(decoding: buf, as: UTF8.self), "pong")
        shutdown(guest[0], SHUT_WR)
        XCTAssertEqual(read(upstream[0], &buf, 4), 0, "half-close must reach the upstream")
        shutdown(upstream[0], SHUT_WR)
        wait(for: [done], timeout: 2)
        for fd in guest + upstream { close(fd) }
    }
}
