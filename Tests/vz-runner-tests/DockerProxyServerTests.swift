import XCTest
@testable import vz_runner

/// Covers the host side of the Docker socket proxy without a VM: unix socket
/// lifecycle (stale path removal, permissions) and client handling when the
/// vsock device is not ready. The byte pump itself needs a real vsock
/// connection and is exercised by the integration suite.
///
/// The server must be held strongly for the whole test: its accept loop only
/// owns a weak self, so a deallocated server stops and removes the socket.
final class DockerProxyServerTests: XCTestCase {
    private var socketPath: String!
    private var server: DockerProxyServer!

    override func setUp() {
        super.setUp()
        // Keep it short: sun_path caps a unix socket path at ~104 bytes.
        socketPath = "/tmp/anvil-test-docker-\(UUID().uuidString).sock"
        // deviceProvider returns nil: the "VM not ready" path.
        server = DockerProxyServer(socketPath: socketPath, deviceProvider: { nil }, resumeProvider: {})
    }

    override func tearDown() {
        server?.stop()
        server = nil
        try? FileManager.default.removeItem(atPath: socketPath)
        super.tearDown()
    }

    private func connectClient() -> Int32 {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return -1 }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        _ = socketPath.withCString { strncpy(&addr.sun_path.0, $0, 103) }
        let ok = withUnsafePointer(to: &addr) { ptr -> Int32 in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard ok == 0 else {
            close(fd)
            return -1
        }
        return fd
    }

    func testStartCreatesRestrictedSocketAndStopRemovesIt() throws {
        server.start()
        XCTAssertTrue(FileManager.default.fileExists(atPath: socketPath))
        // The Docker socket is root-equivalent: owner-only.
        let perms = try FileManager.default.attributesOfItem(atPath: socketPath)[.posixPermissions] as? Int
        XCTAssertEqual(perms, 0o600)
        server.stop()
        XCTAssertFalse(FileManager.default.fileExists(atPath: socketPath))
    }

    func testStartReplacesStaleSocketFile() {
        FileManager.default.createFile(atPath: socketPath, contents: Data("stale".utf8))
        server.start()
        // A leftover socket from a crashed daemon must not break the next one.
        let client = connectClient()
        XCTAssertGreaterThanOrEqual(client, 0, "connect after stale-file replacement failed")
        if client >= 0 { close(client) }
    }

    func testClientWithoutVMGetsEOFAndCallbacks() {
        server.start()

        let connected = expectation(description: "onClientConnect")
        let disconnected = expectation(description: "onClientDisconnect")
        server.onClientConnect = { connected.fulfill() }
        server.onClientDisconnect = { disconnected.fulfill() }

        let client = connectClient()
        XCTAssertGreaterThanOrEqual(client, 0)
        // Server must close the client (VM not ready) — read returns EOF.
        var byte: UInt8 = 0
        let n = read(client, &byte, 1)
        close(client)
        XCTAssertEqual(n, 0, "expected EOF, got \(n) (errno \(errno))")

        wait(for: [connected, disconnected], timeout: 5)
    }

    func testRestartKeepsAcceptingClients() {
        server.start()
        server.stop()
        server.start()
        let client = connectClient()
        XCTAssertGreaterThanOrEqual(client, 0, "connect after restart failed")
        if client >= 0 { close(client) }
    }
}
