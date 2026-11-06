import XCTest
@testable import vz_runner

final class DoctorJSONTests: XCTestCase {
    // jsonEscape must agree with JSONSerialization on every kind of
    // content a check detail can carry, and never force-unwrap.
    func assertEscapesLikeJSON(_ s: String, file: StaticString = #filePath, line: UInt = #line) {
        let escaped = jsonEscape(s)
        XCTAssertEqual(escaped.first, "\"", file: file, line: line)
        XCTAssertEqual(escaped.last, "\"", file: file, line: line)
        // JSONSerialization needs a container at the top level: parse the
        // escaped string the way the original `[s]` wrapper would have.
        let reparsed = (try? JSONSerialization.jsonObject(with: Data("[\(escaped)]".utf8))) as? [String]
        XCTAssertEqual(reparsed?.first, s, file: file, line: line)
    }

    func testPlainString() {
        assertEscapesLikeJSON("hypervisor: OK (1.2.3)")
    }

    func testQuotesBackslashesAndControls() {
        assertEscapesLikeJSON("path \"C:\\Users\\x\"\n\ttab\r\ncr\u{1}ctl")
    }

    func testUnicodePassesThrough() {
        assertEscapesLikeJSON("проверка ✓ 日本語")
    }

    func testEmptyString() {
        assertEscapesLikeJSON("")
    }
}
