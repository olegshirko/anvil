import XCTest
@testable import vz_runner

final class NATRecoveryLadderTests: XCTestCase {
    func testEachRungIsTriedOnceThenGivesUp() {
        var ladder = NATRecoveryLadder()
        XCTAssertEqual(ladder.nextAfterFailure(), .reattachNAT)
        XCTAssertEqual(ladder.nextAfterFailure(), .resetGuestNetwork)
        XCTAssertEqual(ladder.nextAfterFailure(), .giveUp)
        // A path that stays dead (a full-tunnel VPN) must not keep cutting
        // the VM's connections: no rung is repeated.
        XCTAssertEqual(ladder.nextAfterFailure(), .giveUp)
        XCTAssertTrue(ladder.isExhausted)
    }

    func testRecoveryRestoresBothRungs() {
        var ladder = NATRecoveryLadder()
        XCTAssertEqual(ladder.nextAfterFailure(), .reattachNAT)
        XCTAssertEqual(ladder.nextAfterFailure(), .resetGuestNetwork)
        ladder.recovered()
        XCTAssertFalse(ladder.isExhausted)
        XCTAssertEqual(ladder.nextAfterFailure(), .reattachNAT)
        XCTAssertEqual(ladder.nextAfterFailure(), .resetGuestNetwork)
    }

    func testHealthyPathNeverResetsTheGuest() {
        // Nothing advances the ladder without a confirmed failure, so a
        // healthy VM only ever sees probes.
        let ladder = NATRecoveryLadder()
        XCTAssertFalse(ladder.isExhausted)
        XCTAssertEqual(ladder, NATRecoveryLadder())
    }
}
