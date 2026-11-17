import XCTest
@testable import vz_runner

final class SnapshotPolicyTests: XCTestCase {
    private func timings(boot: Double? = nil, restore: Double? = nil,
                         memory: UInt64 = 16) -> SnapshotTimings {
        SnapshotTimings(memoryGiB: memory, coldBootSeconds: boot, restoreSeconds: restore)
    }

    func testRestoreFasterThanBootUsesSnapshot() {
        // 2 GiB: restore 0.406 s vs 0.761 s cold boot.
        let t = timings(boot: 0.761, restore: 0.406)
        XCTAssertTrue(SnapshotPolicy.useSnapshot(mode: .auto, timings: t, memoryGiB: 16))
        XCTAssertTrue(SnapshotPolicy.saveSnapshot(mode: .auto, timings: t, memoryGiB: 16))
    }

    func testRestoreSlowerThanBootSkipsSnapshot() {
        // 16 GiB: restore 2.255 s vs 0.940 s cold boot.
        let t = timings(boot: 0.940, restore: 2.255)
        XCTAssertFalse(SnapshotPolicy.useSnapshot(mode: .auto, timings: t, memoryGiB: 16))
        XCTAssertFalse(SnapshotPolicy.saveSnapshot(mode: .auto, timings: t, memoryGiB: 16))
        XCTAssertEqual(SnapshotPolicy.restoreIsWorthIt(timings: t, memoryGiB: 16), false)
    }

    func testUnknownMeasurementsUseSnapshot() {
        XCTAssertTrue(SnapshotPolicy.useSnapshot(mode: .auto, timings: nil, memoryGiB: 16))
        XCTAssertTrue(SnapshotPolicy.saveSnapshot(mode: .auto, timings: nil, memoryGiB: 16))
        let bootOnly = timings(boot: 0.9)
        let restoreOnly = timings(restore: 2.2)
        XCTAssertNil(SnapshotPolicy.restoreIsWorthIt(timings: bootOnly, memoryGiB: 16))
        XCTAssertNil(SnapshotPolicy.restoreIsWorthIt(timings: restoreOnly, memoryGiB: 16))
        XCTAssertTrue(SnapshotPolicy.useSnapshot(mode: .auto, timings: bootOnly, memoryGiB: 16))
        XCTAssertTrue(SnapshotPolicy.useSnapshot(mode: .auto, timings: restoreOnly, memoryGiB: 16))
    }

    func testMeasurementsOfAnotherMemorySizeAreUnknown() {
        // A memory change invalidates both numbers: restore time tracks RAM.
        let other = timings(boot: 0.9, restore: 5.0, memory: 8)
        XCTAssertNil(SnapshotPolicy.restoreIsWorthIt(timings: other, memoryGiB: 16))
        XCTAssertTrue(SnapshotPolicy.useSnapshot(mode: .auto, timings: other, memoryGiB: 16))
        XCTAssertTrue(SnapshotPolicy.saveSnapshot(mode: .auto, timings: other, memoryGiB: 16))
    }

    func testForcedModes() {
        let slow = timings(boot: 0.9, restore: 2.2)
        XCTAssertTrue(SnapshotPolicy.useSnapshot(mode: .always, timings: slow, memoryGiB: 16))
        XCTAssertTrue(SnapshotPolicy.saveSnapshot(mode: .always, timings: slow, memoryGiB: 16))
        XCTAssertTrue(SnapshotPolicy.useSnapshot(mode: .always, timings: nil, memoryGiB: 16))
        XCTAssertFalse(SnapshotPolicy.useSnapshot(mode: .never, timings: nil, memoryGiB: 16))
        XCTAssertFalse(SnapshotPolicy.saveSnapshot(mode: .never, timings: nil, memoryGiB: 16))
        XCTAssertFalse(SnapshotPolicy.useSnapshot(
            mode: .never, timings: timings(boot: 0.9, restore: 0.4), memoryGiB: 16))
    }

    func testMarginBoundary() {
        // Exactly at the margin still counts as worth it; just above does not.
        XCTAssertTrue(SnapshotPolicy.useSnapshot(
            mode: .auto, timings: timings(boot: 1.0, restore: 1.15), memoryGiB: 16))
        XCTAssertFalse(SnapshotPolicy.useSnapshot(
            mode: .auto, timings: timings(boot: 1.0, restore: 1.1501), memoryGiB: 16))
    }

    func testModeParsing() {
        XCTAssertEqual(SnapshotMode.configured(nil), .auto)
        XCTAssertEqual(SnapshotMode.configured(""), .auto)
        XCTAssertEqual(SnapshotMode.configured("  "), .auto)
        XCTAssertEqual(SnapshotMode.configured("garbage"), .auto)
        XCTAssertEqual(SnapshotMode.configured("auto"), .auto)
        XCTAssertEqual(SnapshotMode.configured(" ALWAYS "), .always)
        XCTAssertEqual(SnapshotMode.configured("Never"), .never)
    }

    func testTimingsStoreRoundTripAndMemoryInvalidation() {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("snapshot-timings-\(UUID().uuidString).json")
        defer { try? FileManager.default.removeItem(at: url) }
        let store = SnapshotTimingsStore(url: url)

        XCTAssertNil(store.load())
        store.recordColdBoot(0.94, memoryGiB: 16)
        XCTAssertEqual(store.load(), SnapshotTimings(memoryGiB: 16, coldBootSeconds: 0.94, restoreSeconds: nil))
        store.recordRestore(2.255, memoryGiB: 16)
        XCTAssertEqual(store.load(), SnapshotTimings(memoryGiB: 16, coldBootSeconds: 0.94, restoreSeconds: 2.255))

        // A measurement for another memory size starts a fresh record.
        store.recordColdBoot(0.76, memoryGiB: 2)
        XCTAssertEqual(store.load(), SnapshotTimings(memoryGiB: 2, coldBootSeconds: 0.76, restoreSeconds: nil))
    }
}
