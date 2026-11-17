import XCTest
@testable import vz_runner

// The duplicate-address warning rests on one parse: `/usr/sbin/arp -n <ip>`
// says which MAC owns the address on the Mac. A wrong parse would either miss
// a real conflict or cry wolf, so the shapes macOS actually prints are pinned
// here.
final class GuestAddressMonitorTests: XCTestCase {
    func testResolvedEntry() {
        let out = """
        ? (192.168.64.2) at fe:50:fb:5e:75:0c on bridge100 ifscope [ethernet]
        ? (192.168.64.1) at 0:1:22:33:44:55 on bridge100 ifscope [ethernet]
        """
        XCTAssertEqual(GuestAddressMonitor.parse(out, ip: "192.168.64.2"), "fe:50:fb:5e:75:0c")
    }

    func testIncompleteEntryIsNotAClaim() {
        let out = "? (192.168.64.2) at (incomplete) on bridge100 ifscope [ethernet]\n"
        XCTAssertNil(GuestAddressMonitor.parse(out, ip: "192.168.64.2"))
    }

    func testOtherAddressesAreIgnored() {
        // Substring traps: .2 must not match .20 or .250.
        let out = """
        ? (192.168.64.20) at aa:bb:cc:dd:ee:01 on bridge100 ifscope [ethernet]
        ? (192.168.64.250) at aa:bb:cc:dd:ee:02 on bridge100 ifscope [ethernet]
        """
        XCTAssertNil(GuestAddressMonitor.parse(out, ip: "192.168.64.2"))
    }

    func testEmptyOutput() {
        XCTAssertNil(GuestAddressMonitor.parse("", ip: "192.168.64.2"))
        XCTAssertNil(GuestAddressMonitor.parse("arp: 192.168.64.2: entry not found\n", ip: "192.168.64.2"))
    }

    func testMACComparisonIgnoresLeadingZeros() {
        // macOS prints ARP without leading zeros; VZ hands out the padded form.
        XCTAssertTrue(GuestAddressMonitor.macEquals("8e:d3:39:2:6a:67", "8e:d3:39:02:6a:67"))
        XCTAssertTrue(GuestAddressMonitor.macEquals("8E:D3:39:02:6A:67", "8e:d3:39:02:6a:67"))
        XCTAssertFalse(GuestAddressMonitor.macEquals("8e:d3:39:02:6a:68", "8e:d3:39:02:6a:67"))
        XCTAssertFalse(GuestAddressMonitor.macEquals("nonsense", "8e:d3:39:02:6a:67"))
    }

    func testNonMACTokenAfterAtIsRejected() {
        // A malformed line must not be mistaken for a MAC.
        let out = "? (192.168.64.2) at nonsense on bridge100 ifscope [ethernet]\n"
        XCTAssertNil(GuestAddressMonitor.parse(out, ip: "192.168.64.2"))
    }
}
