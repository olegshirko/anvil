import XCTest
@testable import vz_runner

final class DoctorRouteTests: XCTestCase {
    func testVMAddressFromIPAddr() {
        let out = "2: eth0    inet 192.168.64.2/24 brd 192.168.64.255 scope global eth0\\       valid_lft forever preferred_lft forever\n"
        XCTAssertEqual(vmAddress(fromIPAddrOutput: out), "192.168.64.2")
        XCTAssertNil(vmAddress(fromIPAddrOutput: ""))
    }
}
