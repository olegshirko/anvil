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
    }
}
