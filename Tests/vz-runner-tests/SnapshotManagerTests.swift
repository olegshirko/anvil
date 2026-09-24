import XCTest
@testable import vz_runner

// The snapshot hash gates restore vs cold boot: any change to kernel,
// initrd, CPU, RAM, disk or the share set must invalidate the snapshot —
// see ARCHITECTURE.md §3.1.
final class SnapshotManagerTests: XCTestCase {
    private var dir: URL!
    private var kernel: URL!
    private var initrd: URL!
    private var disk: URL!

    override func setUpWithError() throws {
        dir = FileManager.default.temporaryDirectory
            .appendingPathComponent("anvil-tests-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        kernel = dir.appendingPathComponent("vmlinuz")
        initrd = dir.appendingPathComponent("initrd")
        disk = dir.appendingPathComponent("disk.img")
        try Data("kernel-v1".utf8).write(to: kernel)
        try Data("initrd-v1".utf8).write(to: initrd)
        try Data(repeating: 0, count: 1024).write(to: disk)
    }

    override func tearDownWithError() throws {
        try FileManager.default.removeItem(at: dir)
    }

    private func makeManager() -> SnapshotManager {
        SnapshotManager(name: "test", directory: dir)
    }

    private func writeHash(_ m: SnapshotManager) {
        XCTAssertTrue(m.writeConfigHash(
            kernel: kernel.path, initrd: initrd.path, cpus: 4, memory: 2,
            containerdDiskPath: disk.path, usersSharePath: "/Users"))
    }

    func testHashRoundtripMatches() {
        let m = makeManager()
        XCTAssertFalse(m.hasSnapshot)
        writeHash(m)
        XCTAssertTrue(m.configHashMatches(
            kernel: kernel.path, initrd: initrd.path, cpus: 4, memory: 2,
            containerdDiskPath: disk.path, usersSharePath: "/Users"))
    }

    /// A snapshot save from a restored session does not re-stamp the config
    /// hash; the pre-save cleanup must therefore keep the hash file, or the
    /// next start sees hasSnapshot == false, cold-boots and wipes containers.
    func testStatePreservingCleanupKeepsConfigHash() throws {
        let m = makeManager()
        writeHash(m)
        try Data("state".utf8).write(to: dir.appendingPathComponent("test.vzstate"))
        XCTAssertTrue(m.hasSnapshot)

        m.removeSnapshotStatePreservingSidecars()

        XCTAssertFalse(FileManager.default.fileExists(
            atPath: dir.appendingPathComponent("test.vzstate").path))
        XCTAssertTrue(m.configHashMatches(
            kernel: kernel.path, initrd: initrd.path, cpus: 4, memory: 2,
            containerdDiskPath: disk.path, usersSharePath: "/Users"),
                      "config hash must survive snapshot-state cleanup")
    }

    // Once a snapshotted VM runs again its memory no longer matches the
    // disk: the saved state must be gone (a crash then cold-boots instead of
    // restoring a stale ext4 view), while the sidecars stay for the next save.
    func testInvalidateBeforeResumeDropsStateKeepsSidecars() throws {
        let m = makeManager()
        writeHash(m)
        try Data("state".utf8).write(to: dir.appendingPathComponent("test.vzstate"))
        XCTAssertTrue(m.saveMachineIdentifier(Data("id".utf8)))

        m.invalidateBeforeResume()

        XCTAssertFalse(m.hasSnapshot, "a resumed VM must not leave a restorable snapshot")
        XCTAssertNotNil(m.loadMachineIdentifier())
        XCTAssertTrue(m.configHashMatches(
            kernel: kernel.path, initrd: initrd.path, cpus: 4, memory: 2,
            containerdDiskPath: disk.path, usersSharePath: "/Users"))
    }

    func testMemoryChangeInvalidates() {
        let m = makeManager()
        writeHash(m)
        XCTAssertFalse(m.configHashMatches(
            kernel: kernel.path, initrd: initrd.path, cpus: 4, memory: 4,
            containerdDiskPath: disk.path, usersSharePath: "/Users"))
    }

    func testCPUChangeInvalidates() {
        let m = makeManager()
        writeHash(m)
        XCTAssertFalse(m.configHashMatches(
            kernel: kernel.path, initrd: initrd.path, cpus: 8, memory: 2,
            containerdDiskPath: disk.path, usersSharePath: "/Users"))
    }

    func testKernelContentChangeInvalidates() throws {
        let m = makeManager()
        writeHash(m)
        try Data("kernel-v2".utf8).write(to: kernel)
        XCTAssertFalse(m.configHashMatches(
            kernel: kernel.path, initrd: initrd.path, cpus: 4, memory: 2,
            containerdDiskPath: disk.path, usersSharePath: "/Users"))
    }

    func testDiskSizeChangeInvalidates() throws {
        let m = makeManager()
        writeHash(m)
        // Grow the disk image — the token includes path:size.
        try Data(repeating: 0, count: 4096).write(to: disk)
        XCTAssertFalse(m.configHashMatches(
            kernel: kernel.path, initrd: initrd.path, cpus: 4, memory: 2,
            containerdDiskPath: disk.path, usersSharePath: "/Users"))
    }

    func testShareSetChangeInvalidates() {
        let m = makeManager()
        writeHash(m)
        XCTAssertFalse(m.configHashMatches(
            kernel: kernel.path, initrd: initrd.path, cpus: 4, memory: 2,
            containerdDiskPath: disk.path, usersSharePath: nil))
    }

    func testMissingDiskChangesToken() throws {
        let m = makeManager()
        writeHash(m)
        try FileManager.default.removeItem(at: disk)
        XCTAssertFalse(m.configHashMatches(
            kernel: kernel.path, initrd: initrd.path, cpus: 4, memory: 2,
            containerdDiskPath: disk.path, usersSharePath: "/Users"))
    }
}
