import XCTest
@testable import vz_runner

// The control protocol is a 4-byte big-endian length prefix + JSON body,
// shared byte-for-byte with the guest-agent over vsock — roundtrip must be
// stable and the 16 MiB guard must hold.
final class ControlProtocolTests: XCTestCase {
    private struct RoundtripCase: Codable, Equatable {
        let cmd: String
        let args: [String]?
    }

    func testEncodeDecodeRoundtrip() throws {
        let value = ControlRequest(cmd: "exec", args: ["ps", "aux"])
        let data = try encodeLengthPrefixed(value)

        let stream = InputStream(data: data)
        stream.open()
        defer { stream.close() }
        let decoded = try decodeLengthPrefixed(ControlRequest.self, from: stream)
        XCTAssertEqual(decoded.cmd, "exec")
        XCTAssertEqual(decoded.args, ["ps", "aux"])
    }

    func testLengthPrefixIsBigEndian() throws {
        let data = try encodeLengthPrefixed(ControlRequest(cmd: "ping", args: nil))
        // First 4 bytes = big-endian body length.
        let bodyLength = Int(data[0]) << 24 | Int(data[1]) << 16 | Int(data[2]) << 8 | Int(data[3])
        XCTAssertEqual(data.count - 4, bodyLength)
    }

    func testEmptyBodyRoundtrip() throws {
        let data = try encodeLengthPrefixed(ControlResponse(stdout: nil, stderr: nil, exitCode: nil, error: nil, status: "ok"))
        let stream = InputStream(data: data)
        stream.open()
        defer { stream.close() }
        let decoded = try decodeLengthPrefixed(ControlResponse.self, from: stream)
        XCTAssertEqual(decoded.status, "ok")
        XCTAssertNil(decoded.exitCode)
    }

    func testOversizeLengthRejected() throws {
        // Hand-craft a frame claiming a 17 MiB body — above the 16 MiB cap.
        var data = Data()
        var length = UInt32(17 * 1024 * 1024).bigEndian
        data.append(Data(bytes: &length, count: 4))
        data.append(Data(repeating: 0x61, count: 8))

        let stream = InputStream(data: data)
        stream.open()
        defer { stream.close() }
        XCTAssertThrowsError(try decodeLengthPrefixed(ControlResponse.self, from: stream))
    }

    func testTruncatedBodyRejected() throws {
        // Claim 100 bytes but provide only 4 — readExactly must hit EOF.
        var data = Data()
        var length = UInt32(100).bigEndian
        data.append(Data(bytes: &length, count: 4))
        data.append(Data(repeating: 0x61, count: 4))

        let stream = InputStream(data: data)
        stream.open()
        defer { stream.close() }
        XCTAssertThrowsError(try decodeLengthPrefixed(ControlResponse.self, from: stream))
    }
}
