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
