import XCTest
@testable import vz_runner

final class RestoreContainersTests: XCTestCase {
    /// Oldest first (a database before the app created after it), names
    /// without Docker's leading slash, entries without an Id skipped.
    func testParseOrdersByCreation() {
        let list: [[String: Any]] = [
            ["Id": "b", "Names": ["/app-1"], "Created": 200],
            ["Names": ["/broken"], "Created": 50],
            ["Id": "a", "Names": ["/db-1"], "Created": 100],
            ["Id": "c", "Created": 100.0],
        ]
        let entries = RestoreContainers.parse(list)
        XCTAssertEqual(entries.map(\.id), ["c", "a", "b"]) // a tie goes by name
        XCTAssertEqual(entries.map(\.name), ["c", "db-1", "app-1"])
    }
}
