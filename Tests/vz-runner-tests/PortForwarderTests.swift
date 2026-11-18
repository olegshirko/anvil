import XCTest
@testable import vz_runner

final class PortForwarderTests: XCTestCase {
    private func sampleMapping(protocol proto: String? = "tcp") -> PortMapping {
        PortMapping(namespace: "ns1", containerID: "abc123", name: "web",
                    hostPort: 8080, containerPort: 80, protocol: proto,
                    guestIP: "192.168.64.2", containerIP: "10.89.0.5")
    }

    func testCodingKeysRoundTrip() throws {
        let data = try JSONEncoder().encode(sampleMapping())
        let json = String(data: data, encoding: .utf8) ?? ""
        // The guest pushes snake_case keys; the Swift field names differ.
        XCTAssertTrue(json.contains("\"container_id\":\"abc123\""))
        XCTAssertTrue(json.contains("\"host_port\":8080"))
        XCTAssertTrue(json.contains("\"guest_ip\":\"192.168.64.2\""))
        let decoded = try JSONDecoder().decode(PortMapping.self, from: data)
        XCTAssertEqual(decoded, sampleMapping())
    }

    func testListenerKeyDistinguishesProtocolAndNamespace() {
        let tcp = sampleMapping().listenerKey
        XCTAssertEqual(tcp, "ns1/abc123/8080/tcp")
        // Missing protocol defaults to tcp on the wire from older guests.
        let unset = sampleMapping(protocol: nil).listenerKey
        XCTAssertEqual(unset, tcp)
        XCTAssertNotEqual(sampleMapping(protocol: "udp").listenerKey, tcp)
        XCTAssertNotEqual(sampleMapping(protocol: "sctp").listenerKey, tcp)
    }

    func testPortMapStateDecodesGuestPayload() throws {
        // Shape pushed by guest-agent portcheck.go.
        let payload = """
        {"mappings":[{"namespace":"compose-proj","container_id":"deadbeef","name":"api",
        "host_port":5432,"container_port":5432,"protocol":"tcp",
        "guest_ip":"192.168.64.2","container_ip":"10.89.1.7"}]}
        """
        let state = try JSONDecoder().decode(PortMapState.self, from: payload.data(using: .utf8)!)
        XCTAssertEqual(state.mappings.count, 1)
        XCTAssertEqual(state.mappings[0].name, "api")
        XCTAssertEqual(state.mappings[0].containerIP, "10.89.1.7")
        XCTAssertNil(state.mappings[0].hostIP, "older guests send no host_ip")
        XCTAssertNil(state.runningContainers, "older guests send no running_containers")
    }

    func testPortMapStateDecodesRunningContainers() throws {
        let payload = #"{"mappings":[],"running_containers":3}"#
        let state = try JSONDecoder().decode(PortMapState.self, from: payload.data(using: .utf8)!)
        XCTAssertEqual(state.runningContainers, 3)
        XCTAssertTrue(state.mappings.isEmpty)
    }

    func testPortMapStateDecodesHostIP() throws {
        let payload = """
        {"mappings":[{"namespace":"default","container_id":"c1","host_port":5432,
        "container_port":5432,"protocol":"tcp","guest_ip":"192.168.64.2","host_ip":"127.0.0.1"}]}
        """
        let state = try JSONDecoder().decode(PortMapState.self, from: payload.data(using: .utf8)!)
        XCTAssertEqual(state.mappings[0].hostIP, "127.0.0.1")
    }

    private func bytes(_ addr: in6_addr) -> [UInt8] {
        withUnsafeBytes(of: addr) { Array($0) }
    }

    func testBindAddressAnyInterface() throws {
        for ip in [nil, "", "0.0.0.0", "::", "[::]"] {
            let bind = try XCTUnwrap(ListenerBindAddress(hostIP: ip), "\(ip ?? "nil")")
            XCTAssertEqual(bytes(bind.address), bytes(in6addr_any))
            XCTAssertFalse(bind.v6Only)
        }
    }

    // -p 127.0.0.1:5432:5432 must bind loopback only, not every interface.
    func testBindAddressIPv4IsMapped() throws {
        let bind = try XCTUnwrap(ListenerBindAddress(hostIP: "127.0.0.1"))
        XCTAssertEqual(bytes(bind.address), [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 127, 0, 0, 1])
        XCTAssertFalse(bind.v6Only, "IPv4-mapped binds need a dual-stack socket")
    }

    func testBindAddressIPv6() throws {
        let bind = try XCTUnwrap(ListenerBindAddress(hostIP: "::1"))
        XCTAssertEqual(bytes(bind.address), bytes(in6addr_loopback))
        XCTAssertTrue(bind.v6Only)
        XCTAssertNotNil(ListenerBindAddress(hostIP: "[::1]"))
    }

    func testBindAddressRejectsGarbage() {
        XCTAssertNil(ListenerBindAddress(hostIP: "localhost"))
        XCTAssertNil(ListenerBindAddress(hostIP: "999.1.1.1"))
    }

    // A loopback listener must not be reachable on another interface.
    func testLoopbackBindAcceptsOnlyLoopback() throws {
        let bindAddr = try XCTUnwrap(ListenerBindAddress(hostIP: "127.0.0.1"))
        let fd = socket(AF_INET6, SOCK_STREAM, 0)
        XCTAssertGreaterThanOrEqual(fd, 0)
        defer { close(fd) }
        XCTAssertEqual(bindAddr.bind(fd: fd, port: 0), 0, String(cString: strerror(errno)))
        XCTAssertEqual(listen(fd, 1), 0)
        var bound = sockaddr_in6()
        var len = socklen_t(MemoryLayout<sockaddr_in6>.size)
        _ = withUnsafeMutablePointer(to: &bound) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &len) }
        }
        XCTAssertEqual(bytes(bound.sin6_addr), [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 127, 0, 0, 1])
    }
}

final class GuestAddressTrackingTests: XCTestCase {
    private func mapping(guestIP: String, containerIP: String = "10.89.0.5") -> PortMapping {
        PortMapping(namespace: "ns", containerID: "c1", name: "web",
                    hostPort: 8080, containerPort: 80, protocol: "tcp",
                    guestIP: guestIP, containerIP: containerIP)
    }

    func testStateAddressWins() {
        // The state-level field is what the guest reads from eth0 every scan.
        let state = PortMapState(mappings: [mapping(guestIP: "192.168.64.2")],
                                 guestIP: "192.168.64.9")
        XCTAssertEqual(resolvedGuestAddress(pushed: state, previous: "192.168.64.2"), "192.168.64.9")
    }

    func testFallsBackToMappingForOlderGuests() {
        let state = PortMapState(mappings: [mapping(guestIP: "192.168.64.4")])
        XCTAssertEqual(resolvedGuestAddress(pushed: state, previous: "192.168.64.2"), "192.168.64.4")
    }

    func testEmptyPushKeepsLastKnownAddress() {
        // A guest mid-deconfig (or one that never reported an address) must
        // not blank every listener: keep dialing the last known address.
        XCTAssertEqual(resolvedGuestAddress(pushed: PortMapState(mappings: []), previous: "192.168.64.3"),
                       "192.168.64.3")
    }

    func testEmptyMappingAddressIsIgnored() {
        let state = PortMapState(mappings: [mapping(guestIP: "")], guestIP: "")
        XCTAssertEqual(resolvedGuestAddress(pushed: state, previous: "192.168.64.3"), "192.168.64.3")
    }

    func testMappingWithGuestAddress() {
        let original = mapping(guestIP: "192.168.64.2")
        XCTAssertEqual(original.withGuestAddress("192.168.64.9").guestIP, "192.168.64.9")
        XCTAssertEqual(original.withGuestAddress("192.168.64.9").containerIP, original.containerIP)
        XCTAssertEqual(original.withGuestAddress(""), original, "an empty push changes nothing")
        XCTAssertEqual(original.withGuestAddress("192.168.64.2"), original)
    }

    func testAddressChangeRebuildsListenersWhoseMappingIsStale() {
        // The restored guest renewed its lease; the push still stamps the old
        // address on the mapping (or the listener predates the change).
        let current = ["ns/c1/8080/tcp": mapping(guestIP: "192.168.64.2")]
        let desired = ["ns/c1/8080/tcp": mapping(guestIP: "192.168.64.2")]
        XCTAssertEqual(listenersToRebuild(current: current, desired: desired, address: "192.168.64.9"),
                       ["ns/c1/8080/tcp"])
    }

    func testAddressChangeDoesNotRebuildUnrelatedKeys() {
        let current = ["ns/c1/8080/tcp": mapping(guestIP: "192.168.64.9")]
        let desired = ["ns/c1/8080/tcp": mapping(guestIP: "192.168.64.9")]
        XCTAssertTrue(listenersToRebuild(current: current, desired: desired, address: "192.168.64.9").isEmpty)
    }

    func testChangedContainerTargetStillRebuilds() {
        let current = ["ns/c1/8080/tcp": mapping(guestIP: "192.168.64.2", containerIP: "10.89.0.5")]
        let desired = ["ns/c1/8080/tcp": mapping(guestIP: "192.168.64.2", containerIP: "10.89.0.6")]
        XCTAssertEqual(listenersToRebuild(current: current, desired: desired, address: "192.168.64.2"),
                       ["ns/c1/8080/tcp"])
    }

    func testNoTrackedAddressKeepsMappingOnlyDecision() {
        // An empty address must not force a rebuild of every listener.
        let current = ["ns/c1/8080/tcp": mapping(guestIP: "192.168.64.2")]
        let desired = ["ns/c1/8080/tcp": mapping(guestIP: "192.168.64.2")]
        XCTAssertTrue(listenersToRebuild(current: current, desired: desired, address: "").isEmpty)
    }
}

final class GuestInterfaceTests: XCTestCase {
    // The forwarder pins guest connections to the interface owning the
    // guest's subnet; loopback is always present to check the lookup.
    func testInterfaceIndexContaining() {
        XCTAssertEqual(interfaceIndex(containing: "127.0.0.1"), if_nametoindex("lo0"))
        XCTAssertNil(interfaceIndex(containing: "not-an-ip"))
        XCTAssertNil(interfaceIndex(containing: "203.0.113.7"), "TEST-NET-3 is on no interface")
    }
}

/// The published-port warm-up: a listener is bound as soon as the guest
/// pushes a mapping, which can be before the guest can serve it. A client
/// arriving then must wait (bounded) rather than get an empty reply.
final class GuestDialRetryTests: XCTestCase {
    private func failure(_ code: Int32) -> GuestDialOutcome {
        GuestDialOutcome(fd: -1, retryable: retryableGuestDialError(code),
                         reason: String(cString: strerror(code)), target: "192.0.2.1:39131")
    }

    func testWarmupErrorsAreRetried() {
        for code in [ECONNREFUSED, ECONNRESET, EHOSTUNREACH, ENETUNREACH,
                     EADDRNOTAVAIL, ETIMEDOUT] {
            XCTAssertTrue(retryableGuestDialError(code), "errno \(code) should retry")
        }
        for code in [EACCES, EPERM, EINVAL, EAFNOSUPPORT] {
            XCTAssertFalse(retryableGuestDialError(code), "errno \(code) should not retry")
        }
    }

    // A real refused connect (the guest's proxy is not listening yet) must
    // land in the retryable set, not just the synthetic errno list.
    func testRefusedConnectIsRetryable() throws {
        // Reserve a loopback port, then release it: nothing listens there.
        let probe = socket(AF_INET, SOCK_STREAM, 0)
        XCTAssertGreaterThanOrEqual(probe, 0)
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = 0
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        let bound = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(probe, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
            }
        }
        XCTAssertEqual(bound, 0, String(cString: strerror(errno)))
        var len = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(probe, $0, &len) }
        }
        close(probe)

        let fd = socket(AF_INET, SOCK_STREAM, 0)
        XCTAssertGreaterThanOrEqual(fd, 0)
        defer { close(fd) }
        let error = try XCTUnwrap(connectWithTimeoutError(fd, addr, timeout: 1.0))
        XCTAssertEqual(error.code, ECONNREFUSED, error.reason)
        XCTAssertTrue(retryableGuestDialError(error.code))
    }

    func testRetriesUntilTheGuestServes() {
        var attempts = 0
        let started = Date()
        let outcome = dialGuestWithRetry(grace: 1.0, retryDelay: 0.02) { _ in
            attempts += 1
            if attempts < 4 { return self.failure(ECONNREFUSED) }
            return GuestDialOutcome(fd: 42, retryable: false, reason: nil, target: "x")
        }
        XCTAssertEqual(outcome.fd, 42)
        XCTAssertEqual(attempts, 4)
        XCTAssertLessThan(Date().timeIntervalSince(started), 1.0)
    }

    func testPermanentErrorIsNotRetried() {
        var attempts = 0
        let outcome = dialGuestWithRetry(grace: 1.0, retryDelay: 0.02) { _ in
            attempts += 1
            return self.failure(EACCES)
        }
        XCTAssertEqual(outcome.fd, -1)
        XCTAssertEqual(attempts, 1)
    }

    func testGivesUpAfterTheGrace() {
        var attempts = 0
        let started = Date()
        let outcome = dialGuestWithRetry(grace: 0.25, retryDelay: 0.05) { _ in
            attempts += 1
            return self.failure(EHOSTUNREACH)
        }
        let elapsed = Date().timeIntervalSince(started)
        XCTAssertEqual(outcome.fd, -1)
        XCTAssertGreaterThanOrEqual(elapsed, 0.25, "the client must be refused only after the grace")
        XCTAssertLessThan(elapsed, 1.0, "the wait stays bounded")
        XCTAssertGreaterThan(attempts, 2)
        XCTAssertEqual(outcome.reason, String(cString: strerror(EHOSTUNREACH)))
    }
}
