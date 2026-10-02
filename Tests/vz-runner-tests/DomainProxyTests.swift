import XCTest
@testable import vz_runner

final class DomainProxyTests: XCTestCase {
    func testHostHeaderParsing() {
        let head = Data("GET / HTTP/1.1\r\nUser-Agent: x\r\nHOST: Web.Shop.anvil.localhost:8080\r\n\r\n".utf8)
        XCTAssertEqual(DomainProxy.hostHeader(head), "web.shop.anvil.localhost")
        XCTAssertNil(DomainProxy.hostHeader(Data("GET / HTTP/1.1\r\n\r\n".utf8)))
        XCTAssertEqual(DomainProxy.hostHeader(Data("GET / HTTP/1.1\r\nHost: api.anvil.localhost.\r\n\r\n".utf8)),
                       "api.anvil.localhost")
    }

    func testContainerName() {
        XCTAssertEqual(DomainProxy.containerName("web.shop.anvil.localhost"), "web.shop")
        XCTAssertEqual(DomainProxy.containerName("db.anvil.localhost"), "db")
        XCTAssertNil(DomainProxy.containerName("anvil.localhost"))
        XCTAssertNil(DomainProxy.containerName("example.com"))
    }

    func testPortStateDecodesDomains() throws {
        let json = #"{"mappings":[],"domains":[{"names":["web","web.shop"],"ip":"10.10.3.2","port":80}],"guest_ip":"192.168.64.5"}"#
        let state = try JSONDecoder().decode(PortMapState.self, from: Data(json.utf8))
        XCTAssertEqual(state.domains, [DomainEntry(names: ["web", "web.shop"], ip: "10.10.3.2", port: 80)])
        XCTAssertEqual(state.guestIP, "192.168.64.5")
    }
}
