import XCTest
@testable import vz_runner

final class HostServicesServerTests: XCTestCase {
    // A service bound only to 127.0.0.1 is reachable — the whole point of
    // the host-loopback path.
    func testDialHostLoopbackReachesLoopbackOnlyService() throws {
        let listener = socket(AF_INET, SOCK_STREAM, 0)
        XCTAssertGreaterThanOrEqual(listener, 0)
        defer { close(listener) }
        var addr = sockaddr_in()
        addr.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_addr.s_addr = in_addr_t(INADDR_LOOPBACK).bigEndian
        var len = socklen_t(MemoryLayout<sockaddr_in>.size)
        let bound = withUnsafeMutablePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(listener, $0, len) == 0 && Darwin.listen(listener, 1) == 0 && getsockname(listener, $0, &len) == 0
            }
        }
        XCTAssertTrue(bound)
        let port = Int(UInt16(bigEndian: addr.sin_port))

        switch dialHostLoopback(port: port, timeoutSeconds: 2) {
        case .success(let fd):
            close(fd)
        case .failure(let error):
            XCTFail("dial 127.0.0.1:\(port): \(error.message)")
        }
    }

    func testDialHostLoopbackRejectsBadPortsAndClosedPorts() {
        if case .success(let fd) = dialHostLoopback(port: 0, timeoutSeconds: 1) {
            close(fd)
            XCTFail("port 0 accepted")
        }
        // Port 1 (tcpmux) is closed on a Mac.
        if case .success(let fd) = dialHostLoopback(port: 1, timeoutSeconds: 1) {
            close(fd)
            XCTFail("closed port reported as connected")
        }
    }

    func testSSHAuthSocketPathPrefersEnvironment() {
        XCTAssertEqual(sshAuthSocketPath(environment: ["SSH_AUTH_SOCK": "/tmp/agent.sock"]), "/tmp/agent.sock")
    }

    func testConnectUnixSocket() throws {
        let path = NSTemporaryDirectory() + "hs-\(getpid()).sock"
        unlink(path)
        defer { unlink(path) }
        let server = socket(AF_UNIX, SOCK_STREAM, 0)
        defer { close(server) }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        _ = path.withCString { strncpy(&addr.sun_path.0, $0, MemoryLayout.size(ofValue: addr.sun_path) - 1) }
        let ok = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(server, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0 && Darwin.listen(server, 1) == 0
            }
        }
        XCTAssertTrue(ok)
        guard let fd = connectUnixSocket(path) else {
            return XCTFail("connect: \(String(cString: strerror(errno)))")
        }
        close(fd)
        XCTAssertNil(connectUnixSocket(path + ".missing"))
    }
}
